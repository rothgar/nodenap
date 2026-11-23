package planner

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/energy"
)

var now = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)

type machine struct {
	name      string
	arch      string
	cpu, mem  string
	gpu       int
	idleW     float64
	wpc       float64
	on        bool
	managed   bool
	protected bool
	taints    []corev1.Taint
}

// fleet mirrors a real heterogeneous homelab.
func fleet() []machine {
	return []machine{
		{name: "mini", arch: "amd64", cpu: "12", mem: "28Gi", gpu: 2, idleW: 15, wpc: 3, on: true, managed: true, protected: true},
		{name: "pi4-1", arch: "arm64", cpu: "4", mem: "7Gi", idleW: 4, wpc: 1, on: true, managed: true},
		{name: "pi4-2", arch: "arm64", cpu: "4", mem: "7Gi", idleW: 4, wpc: 1, on: false, managed: true},
		{name: "spark", arch: "arm64", cpu: "20", mem: "120Gi", gpu: 2, idleW: 40, wpc: 6, on: false, managed: true},
		{name: "qb2", arch: "amd64", cpu: "16", mem: "240Gi", idleW: 120, wpc: 8, on: false, managed: true},
	}
}

func build(ms []machine) []*Node {
	var out []*Node
	for _, m := range ms {
		alloc := corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(m.cpu),
			corev1.ResourceMemory: resource.MustParse(m.mem),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		if m.gpu > 0 {
			alloc["nvidia.com/gpu"] = *resource.NewQuantity(int64(m.gpu), resource.DecimalSI)
		}
		lbls := map[string]string{"kubernetes.io/arch": m.arch, "kubernetes.io/hostname": m.name}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: m.name, Labels: lbls},
			Spec:       corev1.NodeSpec{Taints: m.taints},
			Status:     corev1.NodeStatus{Allocatable: alloc, Capacity: alloc},
		}
		n := &Node{
			Name: m.name, Managed: m.managed, Protected: m.protected,
			CanPowerOn: true, CanPowerOff: true,
			Ready:   m.on,
			Node:    node,
			Cached:  &v1alpha1.CachedNode{Labels: lbls, Taints: m.taints, Allocatable: alloc},
			Profile: Profile{IdleWatts: m.idleW, WattsPerCore: m.wpc, BootTime: time.Minute},
		}
		if m.on {
			n.State, n.Target = v1alpha1.PowerOn, v1alpha1.TargetOn
		} else {
			n.State, n.Target = v1alpha1.PowerOff, v1alpha1.TargetOff
			n.Node.Spec.Unschedulable = true
			n.Node.Spec.Taints = append(n.Node.Spec.Taints, corev1.Taint{Key: v1alpha1.TaintKey, Effect: corev1.TaintEffectNoSchedule})
		}
		out = append(out, n)
	}
	return out
}

func pod(name, cpu, mem string, opts ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: name + "-rs", Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
		}}}},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func gpu(n int64) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"] = *resource.NewQuantity(n, resource.DecimalSI)
	}
}

func arch(a string) func(*corev1.Pod) {
	return func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": a} }
}

func settings() Settings {
	return Settings{UtilizationThreshold: 0.5, UnneededTime: 10 * time.Minute, MaxConcurrentDown: 1}
}

func grid(price float64) energy.Snapshot { return energy.Snapshot{Now: now, GridPrice: price} }

func targets(r Result) map[string]v1alpha1.Target {
	m := map[string]v1alpha1.Target{}
	for _, a := range r.Actions {
		m[a.Node] = a.Target
	}
	return m
}

func byName(nodes []*Node, name string) *Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// fill makes a node busy so pods can't move onto it.
func fill(n *Node, cpu string) {
	n.Pods = append(n.Pods, pod(n.Name+"-busy", cpu, "1Gi"))
}

func TestScaleUpWakesOnlyMachineWithGPU(t *testing.T) {
	nodes := build(fleet())
	fill(byName(nodes, "mini"), "11")
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("llm", "4", "32Gi", gpu(1), arch("arm64"))}})
	got := targets(r)
	if len(got) != 1 || got["spark"] != v1alpha1.TargetOn {
		t.Fatalf("want only spark woken, got %v (notes %v, unsat %v)", got, r.Notes, r.Unsatisfiable)
	}
}

func TestScaleUpWakesBigMemoryMachine(t *testing.T) {
	nodes := build(fleet())
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("db", "2", "200Gi")}})
	if got := targets(r); len(got) != 1 || got["qb2"] != v1alpha1.TargetOn {
		t.Fatalf("want qb2, got %v", got)
	}
}

func TestScaleUpPrefersCheapestMachine(t *testing.T) {
	nodes := build(fleet())
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("web", "1", "1Gi")}})
	if got := targets(r); len(got) != 1 || got["pi4-2"] != v1alpha1.TargetOn {
		t.Fatalf("want pi4-2 (4W) over spark/qb2, got %v", got)
	}
}

func TestScaleUpPacksManyPodsOnOneEfficientMachine(t *testing.T) {
	nodes := build(fleet())
	var pods []*corev1.Pod
	for i := range 10 {
		pods = append(pods, pod("w"+string(rune('a'+i)), "1500m", "4Gi"))
	}
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(), PendingPods: pods})
	got := targets(r)
	// 10×1.5 cores = 15 cores: spark (20 cores, 40W+6W/core) serves all of
	// them for less per pod than several Pis + qb2.
	if got["spark"] != v1alpha1.TargetOn || len(r.Unsatisfiable) != 0 {
		t.Fatalf("want spark woken for the batch, got %v unsat %v", got, r.Unsatisfiable)
	}
}

func TestUnsatisfiablePodIsReported(t *testing.T) {
	nodes := build(fleet())
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("huge", "2", "1Ti")}})
	if len(r.Actions) != 0 {
		t.Fatalf("expected no actions, got %v", r.Actions)
	}
	if len(r.Unsatisfiable) != 1 || !strings.Contains(r.Unsatisfiable[0], "insufficient memory") {
		t.Fatalf("expected unsatisfiable memory reason, got %v", r.Unsatisfiable)
	}
}

func TestUntoleratedTaintSkipsMachine(t *testing.T) {
	ms := fleet()
	for i := range ms {
		if ms[i].name == "pi4-2" {
			ms[i].taints = []corev1.Taint{{Key: "dedicated", Value: "radio", Effect: corev1.TaintEffectNoSchedule}}
		}
	}
	nodes := build(ms)
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("web", "1", "1Gi")}})
	if got := targets(r); got["pi4-2"] != "" {
		t.Fatalf("pi4-2 is tainted and should not be woken: %v", got)
	}
	tolerant := pod("radio", "1", "1Gi", func(p *corev1.Pod) {
		p.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "radio", Effect: corev1.TaintEffectNoSchedule}}
	})
	r = Plan(Input{Now: now, Nodes: build(ms), Energy: grid(0.30), Settings: settings(), PendingPods: []*corev1.Pod{tolerant}})
	if got := targets(r); got["pi4-2"] != v1alpha1.TargetOn {
		t.Fatalf("tolerating pod should wake pi4-2: %v", got)
	}
}

func TestBootingMachineAbsorbsPendingPods(t *testing.T) {
	nodes := build(fleet())
	sp := byName(nodes, "spark")
	sp.State, sp.Target = v1alpha1.PowerBooting, v1alpha1.TargetOn
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("llm", "4", "32Gi", gpu(1))}})
	if len(r.Actions) != 0 {
		t.Fatalf("spark is already booting; want no actions, got %v", r.Actions)
	}
}

func TestMaxConcurrentUp(t *testing.T) {
	nodes := build(fleet())
	s := settings()
	s.MaxConcurrentUp = 1
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: s,
		PendingPods: []*corev1.Pod{pod("db", "2", "200Gi"), pod("llm", "4", "32Gi", gpu(1))}})
	if len(r.Actions) != 1 {
		t.Fatalf("want 1 action with maxConcurrentUp=1, got %v", r.Actions)
	}
}

func scaleDownFleet() []*Node {
	ms := fleet()
	for i := range ms {
		ms[i].on = true
	}
	return build(ms)
}

func TestScaleDownLeastEfficientFirst(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	got := targets(r)
	// qb2 draws 120W idle for 16 cores (7.5 W/core) – the worst ratio.
	if len(got) != 1 || got["qb2"] != v1alpha1.TargetOff {
		t.Fatalf("want qb2 off first, got %v notes %v", got, r.Notes)
	}
}

func TestScaleDownWaitsForUnneededTime(t *testing.T) {
	nodes := scaleDownFleet()
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if len(r.Actions) != 0 {
		t.Fatalf("nodes just became unneeded; want no actions, got %v", r.Actions)
	}
	if _, ok := r.Unneeded["qb2"]; !ok {
		t.Fatalf("qb2 should be tracked as unneeded: %v", r.Unneeded)
	}
	if _, ok := r.Unneeded["mini"]; ok {
		t.Fatalf("protected mini must never be unneeded")
	}
}

func TestScaleDownRespectsPodPlacement(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	// A GPU pod on spark can only move to mini, and mini is full.
	fill(byName(nodes, "mini"), "11")
	byName(nodes, "mini").Pods[0].Spec.Containers[0].Resources.Requests["nvidia.com/gpu"] = resource.MustParse("2")
	byName(nodes, "spark").Pods = []*corev1.Pod{pod("llm", "1", "1Gi", gpu(1))}
	s := settings()
	s.MaxConcurrentDown = 10
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: s})
	if targets(r)["spark"] != "" {
		t.Fatalf("spark's GPU pod has nowhere to go; spark must stay on: %v", r.Actions)
	}
}

func TestScaleDownBlockedByBarePod(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	bare := pod("bare", "100m", "100Mi")
	bare.OwnerReferences = nil
	byName(nodes, "qb2").Pods = []*corev1.Pod{bare}
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if targets(r)["qb2"] != "" {
		t.Fatalf("bare pod should block qb2: %v", r.Actions)
	}
}

func TestSolarSurplusKeepsMachinesOn(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	s := settings()
	s.MinSavingsPerHour = 0.001
	surplus := 3000.0
	e := energy.Snapshot{Now: now, GridPrice: 0.30, ExportPrice: 0, GridPowerWatts: &surplus}
	r := Plan(Input{Now: now, Nodes: nodes, Energy: e, Settings: s})
	if len(r.Actions) != 0 {
		t.Fatalf("with free solar nothing should be powered off, got %v", r.Actions)
	}
	// At night the same fleet consolidates.
	r = Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: s})
	if len(r.Actions) == 0 {
		t.Fatalf("expected scale-down at grid price")
	}
}

func TestMinNodesOn(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	s := settings()
	s.MinNodesOn = 5
	s.MaxConcurrentDown = 10
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: s})
	if len(r.Actions) != 0 {
		t.Fatalf("minNodesOn=5 with 5 nodes on: want no actions, got %v", r.Actions)
	}
}

func TestNoScaleDownRightAfterScaleUp(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	s := settings()
	s.DelayAfterScaleUp = 10 * time.Minute
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: s, LastScaleUp: now.Add(-time.Minute)})
	if len(r.Actions) != 0 {
		t.Fatalf("want no scale-down right after scale-up, got %v", r.Actions)
	}
}

func TestHeadroomWakesCheapestCapacity(t *testing.T) {
	nodes := build(fleet())
	fill(byName(nodes, "mini"), "11")
	fill(byName(nodes, "pi4-1"), "3500m")
	s := settings()
	s.HeadroomCPU = ptr.To(resource.MustParse("2"))
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: s})
	if got := targets(r); len(got) != 1 || got["pi4-2"] != v1alpha1.TargetOn {
		t.Fatalf("want pi4-2 woken for headroom, got %v", got)
	}
}

func TestOverrideOn(t *testing.T) {
	nodes := build(fleet())
	byName(nodes, "qb2").Override = v1alpha1.TargetOn
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if targets(r)["qb2"] != v1alpha1.TargetOn {
		t.Fatalf("override should wake qb2: %v", r.Actions)
	}
}

func TestDaemonSetOverheadReservedOnOffNodes(t *testing.T) {
	nodes := build(fleet())
	// pi4-2 has 4 cores but DaemonSets take 3.5 of them.
	byName(nodes, "pi4-2").Cached.DaemonSetRequests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3500m")}
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("web", "1", "1Gi", arch("arm64"))}})
	if got := targets(r); got["pi4-2"] != "" || got["spark"] != v1alpha1.TargetOn {
		t.Fatalf("pi4-2 has no room after DaemonSets; want spark, got %v", got)
	}
}

func ignoreLabel(p *corev1.Pod) bool {
	return p.Labels["app.kubernetes.io/name"] == "tt-fabric-manager"
}

func ttManager() *corev1.Pod {
	return pod("tt-fabric-manager", "100m", "128Mi", func(p *corev1.Pod) {
		p.Labels = map[string]string{"app.kubernetes.io/name": "tt-fabric-manager"}
		p.Spec.NodeSelector = map[string]string{"tenstorrent.com/has-tt": "true"}
	})
}

func TestIgnoredPendingPodDoesNotWake(t *testing.T) {
	nodes := build(fleet())
	byName(nodes, "qb2").Cached.Labels["tenstorrent.com/has-tt"] = "true"
	in := Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(), PendingPods: []*corev1.Pod{ttManager()}}
	if got := targets(Plan(in)); got["qb2"] != v1alpha1.TargetOn {
		t.Fatalf("without ignore qb2 should wake: %v", got)
	}
	in.Nodes = build(fleet())
	byName(in.Nodes, "qb2").Cached.Labels["tenstorrent.com/has-tt"] = "true"
	in.Ignored = ignoreLabel
	r := Plan(in)
	if len(r.Actions) != 0 || len(r.Unsatisfiable) != 0 {
		t.Fatalf("ignored pod must not wake or be reported: %v %v", r.Actions, r.Unsatisfiable)
	}
}

func TestIgnoredPodDoesNotBlockPowerOff(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	qb2 := byName(nodes, "qb2")
	qb2.Node.Labels["tenstorrent.com/has-tt"] = "true"
	// The manager can only run on qb2, so it pins qb2 on…
	qb2.Pods = []*corev1.Pod{ttManager()}
	if got := targets(Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})); got["qb2"] != "" {
		t.Fatalf("unignored manager should keep qb2 on: %v", got)
	}
	// …unless it's ignored.
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(), Ignored: ignoreLabel})
	if got := targets(r); got["qb2"] != v1alpha1.TargetOff {
		t.Fatalf("ignored manager should not block qb2: %v notes %v", got, r.Notes)
	}
}
