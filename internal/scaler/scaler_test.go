package scaler

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// harness runs the real loop against a fake API server, with machines
// switched through the real HTTP driver hitting a recording test server.
type harness struct {
	t     *testing.T
	c     client.Client
	s     *Scaler
	now   time.Time
	mu    sync.Mutex
	calls []string
	srv   *httptest.Server
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	h := &harness{t: t, now: time.Now().Truncate(time.Second)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, strings.TrimPrefix(r.URL.Path, "/"))
		h.mu.Unlock()
	}))
	t.Cleanup(h.srv.Close)
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	h.c = fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Machine{}, &v1alpha1.PowerPolicy{}).Build()
	h.s = &Scaler{Client: h.c, Namespace: "nodenap", Now: func() time.Time { return h.now }}
	return h
}

func (h *harness) url(action string) *v1alpha1.Driver {
	return &v1alpha1.Driver{HTTP: &v1alpha1.HTTPRequest{URL: h.srv.URL + "/" + action}}
}

func (h *harness) machine(name string, mut ...func(*v1alpha1.Machine)) *v1alpha1.Machine {
	m := &v1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.MachineSpec{
		PowerOn:  []v1alpha1.Driver{*h.url(name + "/on")},
		Shutdown: h.url(name + "/shutdown"),
		PowerCut: h.url(name + "/cut"),
	}}
	for _, f := range mut {
		f(m)
	}
	return m
}

func policy(dry bool) *v1alpha1.PowerPolicy {
	return &v1alpha1.PowerPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: v1alpha1.PowerPolicySpec{DryRun: dry}}
}

func node(name string, ready bool, cpu, mem string, lbls map[string]string) *corev1.Node {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	alloc := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem), corev1.ResourcePods: resource.MustParse("110")}
	if lbls == nil {
		lbls = map[string]string{}
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls},
		Status: corev1.NodeStatus{Allocatable: alloc, Capacity: alloc,
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour))}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.9"}}}}
}

func pod(name, nodeName, owner string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: owner, Name: "x", UID: "u", Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func (h *harness) run() {
	h.t.Helper()
	if _, err := h.s.RunOnce(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) get(obj client.Object, name string) {
	h.t.Helper()
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: obj.GetNamespace()}, obj); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) m(name string) *v1alpha1.Machine {
	var m v1alpha1.Machine
	h.get(&m, name)
	return &m
}

func (h *harness) n(name string) *corev1.Node {
	var n corev1.Node
	h.get(&n, name)
	return &n
}

func (h *harness) override(name, v string) {
	h.t.Helper()
	m := h.m(name)
	m.Annotations = map[string]string{v1alpha1.OverrideAnnotation: v}
	if err := h.c.Update(context.Background(), m); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) setReady(name string, ready bool) {
	h.t.Helper()
	n := h.n(name)
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st, LastTransitionTime: metav1.NewTime(h.now)}}
	if err := h.c.Status().Update(context.Background(), n); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) state(name string, want v1alpha1.PowerState) {
	h.t.Helper()
	if got := h.m(name).Status.PowerState; got != want {
		h.t.Fatalf("%s: state %s, want %s (%s)", name, got, want, h.m(name).Status.Message)
	}
}

func (h *harness) gotCalls() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.calls, ",")
}

func podExists(h *harness, name string) bool {
	var p corev1.Pod
	return h.c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &p) == nil
}

func TestFullPowerCycle(t *testing.T) {
	h := newHarness(t, policy(false), node("cp", true, "4", "8Gi", map[string]string{"node-role.kubernetes.io/control-plane": ""}),
		node("gpu", true, "20", "120Gi", map[string]string{"kubernetes.io/arch": "arm64"}), pod("web", "gpu", "ReplicaSet"))
	if err := h.c.Create(context.Background(), h.machine("gpu")); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.state("gpu", v1alpha1.PowerOn)
	if c := h.m("gpu").Status.CachedNode; c == nil || c.Labels["kubernetes.io/arch"] != "arm64" {
		t.Fatalf("capabilities not cached: %+v", c)
	}

	h.override("gpu", "off")
	h.run() // target Off → Draining
	h.state("gpu", v1alpha1.PowerDraining)
	if PowerTaintEffect(h.n("gpu")) != corev1.TaintEffectNoSchedule || !h.n("gpu").Spec.Unschedulable {
		t.Fatal("draining node should be cordoned with a NoSchedule taint")
	}
	h.run() // evicts web
	h.run() // nothing left → shutdown
	h.state("gpu", v1alpha1.PowerShuttingDown)

	h.setReady("gpu", false)
	h.run() // inside powerCutDelay: wait
	h.state("gpu", v1alpha1.PowerShuttingDown)
	h.now = h.now.Add(time.Minute)
	h.run() // cut power → Off
	h.state("gpu", v1alpha1.PowerOff)
	h.run() // Off pass: NoExecute
	if PowerTaintEffect(h.n("gpu")) != corev1.TaintEffectNoExecute || h.m("gpu").Status.PoweredOffAt == nil {
		t.Fatal("off machine should carry NoExecute and poweredOffAt")
	}

	h.override("gpu", "on")
	h.run() // Off → Booting, powerOn sent
	h.state("gpu", v1alpha1.PowerBooting)
	// While booting the taint is NoSchedule again so DaemonSets that only
	// tolerate NoSchedule (e.g. flannel, the CNI) can start; otherwise the
	// node might never become Ready.
	if PowerTaintEffect(h.n("gpu")) != corev1.TaintEffectNoSchedule || !h.n("gpu").Spec.Unschedulable {
		t.Fatalf("booting node should be cordoned with NoSchedule, got %q", PowerTaintEffect(h.n("gpu")))
	}
	h.setReady("gpu", true)
	h.run()
	h.state("gpu", v1alpha1.PowerOn)
	if n := h.n("gpu"); PowerTaintEffect(n) != "" || n.Spec.Unschedulable {
		t.Fatal("woken node should be untainted and uncordoned")
	}
	if got := h.gotCalls(); !strings.HasPrefix(got, "gpu/shutdown,gpu/cut,gpu/on") {
		t.Fatalf("driver calls %s", got)
	}
}

func TestDrainCancelled(t *testing.T) {
	h := newHarness(t, policy(false), node("a", true, "4", "8Gi", nil), pod("web", "a", "ReplicaSet"))
	_ = h.c.Create(context.Background(), h.machine("a"))
	h.run()
	h.override("a", "off")
	h.run()
	h.state("a", v1alpha1.PowerDraining)
	h.override("a", "on")
	h.run()
	h.state("a", v1alpha1.PowerOn)
	if PowerTaintEffect(h.n("a")) != "" || h.n("a").Spec.Unschedulable {
		t.Fatal("cancelled drain must untaint and uncordon")
	}
}

func TestBootFallbackFailureAndPerMachineTimeout(t *testing.T) {
	h := newHarness(t, policy(false), node("big", false, "16", "240Gi", nil))
	m := h.machine("big", func(m *v1alpha1.Machine) {
		m.Spec.PowerOn = []v1alpha1.Driver{*h.url("big/wol"), *h.url("big/plug")}
		m.Spec.BootTimeout = &metav1.Duration{Duration: 6 * time.Minute}
		m.Annotations = map[string]string{v1alpha1.OverrideAnnotation: "on"}
	})
	_ = h.c.Create(context.Background(), m)
	h.run() // adopt: Off
	h.run() // target On → Booting via powerOn[0]
	h.state("big", v1alpha1.PowerBooting)
	h.now = h.now.Add(5 * time.Minute)
	h.run()
	if a := h.m("big").Status.PowerOnAttempt; a != 0 {
		t.Fatalf("within the machine's 6m bootTimeout; attempt=%d", a)
	}
	h.now = h.now.Add(2 * time.Minute)
	h.run()
	if a := h.m("big").Status.PowerOnAttempt; a != 1 {
		t.Fatalf("next driver after timeout; attempt=%d", a)
	}
	h.now = h.now.Add(7 * time.Minute)
	h.run()
	h.state("big", v1alpha1.PowerFailed)
	h.now = h.now.Add(failureBackoff(1) + time.Second)
	h.run() // Failed → Off
	h.run() // Off → Booting again
	h.state("big", v1alpha1.PowerBooting)
	if got := h.gotCalls(); !strings.Contains(got, "big/wol") || !strings.Contains(got, "big/plug") {
		t.Fatalf("both drivers should be tried: %s", got)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	h := newHarness(t, policy(true), node("a", true, "4", "8Gi", nil), node("b", false, "4", "8Gi", nil), pod("web", "a", "ReplicaSet"))
	_ = h.c.Create(context.Background(), h.machine("a", func(m *v1alpha1.Machine) { m.Annotations = map[string]string{v1alpha1.OverrideAnnotation: "off"} }))
	_ = h.c.Create(context.Background(), h.machine("b"))
	for range 3 {
		h.run()
	}
	h.state("a", v1alpha1.PowerOn)
	h.state("b", v1alpha1.PowerOff)
	for _, n := range []string{"a", "b"} {
		if PowerTaintEffect(h.n(n)) != "" || h.n(n).Spec.Unschedulable {
			t.Fatalf("dry run changed node %s", n)
		}
	}
	if !podExists(h, "web") || h.gotCalls() != "" {
		t.Fatalf("dry run evicted pods or called drivers: %s", h.gotCalls())
	}
	var p v1alpha1.PowerPolicy
	h.get(&p, "default")
	if len(p.Status.LastActions) != 1 || p.Status.LastActions[0].Machine != "a" {
		t.Fatalf("dry run should report the decision: %+v", p.Status.LastActions)
	}
}

// A dry-run decision must be repeated every loop, not alternate with the
// next-best machine while the first one's unneeded timer restarts.
func TestDryRunDecisionIsStable(t *testing.T) {
	h := newHarness(t, policy(true), node("cp", true, "4", "8Gi", nil),
		node("big", true, "16", "64Gi", nil), node("small", true, "8", "16Gi", nil))
	for name, w := range map[string]string{"big": "500", "small": "40"} {
		_ = h.c.Create(context.Background(), h.machine(name, func(m *v1alpha1.Machine) { m.Spec.Profile.IdleWatts = w }))
	}
	h.run()
	for i := range 4 {
		h.now = h.now.Add(16 * time.Minute)
		h.run()
		var p v1alpha1.PowerPolicy
		h.get(&p, "default")
		if len(p.Status.LastActions) != 1 || p.Status.LastActions[0].Machine != "big" {
			t.Fatalf("loop %d: want big powered off every time, got %+v", i, p.Status.LastActions)
		}
	}
}

// The controller can power off its own node: it evicts every other pod
// first, then itself, and a replacement elsewhere finishes the job.
func TestPowersOffItsOwnNode(t *testing.T) {
	ctrlPod := pod("nodenap-abc", "gpu", "ReplicaSet")
	h := newHarness(t, policy(false), node("cp", true, "4", "8Gi", map[string]string{"node-role.kubernetes.io/control-plane": ""}),
		node("gpu", true, "20", "120Gi", nil), pod("web", "gpu", "ReplicaSet"), ctrlPod)
	h.s.SelfPod = "default/nodenap-abc"
	if err := h.c.Create(context.Background(), h.machine("gpu")); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.override("gpu", "off")
	h.run() // target Off → Draining
	h.state("gpu", v1alpha1.PowerDraining)
	h.run() // evicts web, keeps itself
	if podExists(h, "web") || !podExists(h, "nodenap-abc") {
		t.Fatalf("want web evicted first and the controller kept: web=%v self=%v", podExists(h, "web"), podExists(h, "nodenap-abc"))
	}
	h.run() // only itself left → evicts itself
	if podExists(h, "nodenap-abc") {
		t.Fatal("controller should evict itself once it is the last pod")
	}

	// The replacement runs elsewhere with fresh memory and resumes from status.
	h.s = &Scaler{Client: h.c, Namespace: "nodenap", SelfPod: "default/nodenap-xyz", Now: func() time.Time { return h.now }}
	h.run()
	h.state("gpu", v1alpha1.PowerShuttingDown)
	if !strings.Contains(h.gotCalls(), "gpu/shutdown") {
		t.Fatalf("replacement should send the shutdown, calls: %s", h.gotCalls())
	}
}

func TestNoPolicyObservesOnly(t *testing.T) {
	h := newHarness(t, node("b", false, "4", "8Gi", nil))
	_ = h.c.Create(context.Background(), h.machine("b"))
	h.run()
	h.state("b", v1alpha1.PowerOff)
	if PowerTaintEffect(h.n("b")) != "" || h.gotCalls() != "" {
		t.Fatal("without a PowerPolicy nothing may be touched")
	}
}

func TestManualCordonSurvivesWake(t *testing.T) {
	n := node("b", false, "4", "8Gi", nil)
	n.Spec.Unschedulable = true // a person cordoned it
	h := newHarness(t, policy(false), n)
	_ = h.c.Create(context.Background(), h.machine("b"))
	h.run() // adopt Off
	h.run() // Off: NoExecute taint, node already cordoned
	h.override("b", "on")
	h.run()
	h.setReady("b", true)
	h.run()
	h.state("b", v1alpha1.PowerOn)
	if got := h.n("b"); PowerTaintEffect(got) != "" || !got.Spec.Unschedulable {
		t.Fatal("our taint must go but the manual cordon must stay")
	}
}

func TestStrandedPods(t *testing.T) {
	poweredOff := func(m *v1alpha1.Machine) {
		m.Status = v1alpha1.MachineStatus{PowerState: v1alpha1.PowerOff, Target: v1alpha1.TargetOff,
			PoweredOffAt: &metav1.Time{Time: time.Now().Add(-time.Hour)}, LastTransitionTime: &metav1.Time{Time: time.Now().Add(-time.Hour)}}
		m.Annotations = map[string]string{v1alpha1.OverrideAnnotation: "off"}
	}
	h := newHarness(t, policy(false), node("b", false, "4", "8Gi", nil), pod("nfs-controller", "b", "ReplicaSet"), pod("kube-proxy", "b", "DaemonSet"))
	_ = h.c.Create(context.Background(), h.machine("b", poweredOff))
	m := h.m("b")
	m.Status = h.machine("b", poweredOff).Status
	_ = h.c.Status().Update(context.Background(), m)
	h.run()
	if podExists(h, "nfs-controller") || !podExists(h, "kube-proxy") {
		t.Fatal("stranded ReplicaSet pod should be force-deleted; DaemonSet pod kept")
	}

	// Found NotReady (not powered off by us): never force-delete.
	h2 := newHarness(t, policy(false), node("c", false, "4", "8Gi", nil), pod("postgres-0", "c", "StatefulSet"))
	_ = h2.c.Create(context.Background(), h2.machine("c"))
	h2.run()
	h2.run()
	if !podExists(h2, "postgres-0") || !strings.Contains(h2.m("c").Status.Message, "not force-deleting") {
		t.Fatalf("unconfirmed power state must not force-delete: %q", h2.m("c").Status.Message)
	}
}

func TestPendingPodWakesTheMachineThatFits(t *testing.T) {
	cp := node("cp", true, "4", "8Gi", map[string]string{"node-role.kubernetes.io/control-plane": ""})
	big, small := node("big", false, "16", "240Gi", nil), node("small", false, "4", "8Gi", nil)
	db := pod("db", "", "ReplicaSet")
	db.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("100Gi")
	db.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled,
		Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute))}}}
	h := newHarness(t, policy(false), cp, big, small, db)
	for _, nm := range []string{"big", "small"} {
		m := h.machine(nm)
		_ = h.c.Create(context.Background(), m)
		m = h.m(nm)
		m.Status = v1alpha1.MachineStatus{PowerState: v1alpha1.PowerOff, Target: v1alpha1.TargetOff,
			CachedNode: Snapshot(map[string]*corev1.Node{"big": big, "small": small}[nm], nil, time.Now())}
		_ = h.c.Status().Update(context.Background(), m)
	}
	h.run()
	if h.m("big").Status.Target != v1alpha1.TargetOn || h.m("small").Status.Target != v1alpha1.TargetOff {
		t.Fatalf("only big fits: big=%s small=%s", h.m("big").Status.Target, h.m("small").Status.Target)
	}
	if h.gotCalls() != "big/on" {
		t.Fatalf("calls %s", h.gotCalls())
	}
}

func TestIgnoreMatcher(t *testing.T) {
	m, err := ignoreMatcher([]v1alpha1.PodMatcher{{Namespace: "hw", Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "mgr"}}}})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(ns string, l, a map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Labels: l, Annotations: a}}
	}
	if !m(mk("hw", map[string]string{"app": "mgr"}, nil)) || m(mk("x", map[string]string{"app": "mgr"}, nil)) ||
		!m(mk("x", nil, map[string]string{v1alpha1.IgnorePodAnnotation: "true"})) {
		t.Fatal("matcher")
	}
	if _, err := ignoreMatcher([]v1alpha1.PodMatcher{{}}); err == nil {
		t.Fatal("an empty matcher would match everything")
	}
}

func TestSavingsAccountingAndTelemetry(t *testing.T) {
	var got []Report
	var mu sync.Mutex
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			t.Errorf("bad report: %v", err)
		}
		mu.Lock()
		got = append(got, rep)
		mu.Unlock()
	}))
	defer collector.Close()

	p := policy(false)
	p.Spec.Energy.Tariff = &v1alpha1.Tariff{DefaultPrice: "0.30"}
	p.Spec.Telemetry = v1alpha1.Telemetry{Enabled: true, Endpoint: collector.URL}
	h := newHarness(t, p, node("big", false, "16", "64Gi", nil), node("found-off", false, "4", "8Gi", nil))
	off := func(poweredByUs bool) func(*v1alpha1.Machine) {
		return func(m *v1alpha1.Machine) {
			m.Annotations = map[string]string{v1alpha1.OverrideAnnotation: "off"}
			m.Spec.Profile = v1alpha1.Profile{IdleWatts: "100", OffWatts: "2"}
		}
	}
	for _, name := range []string{"big", "found-off"} {
		_ = h.c.Create(context.Background(), h.machine(name, off(name == "big")))
		m := h.m(name)
		m.Status = v1alpha1.MachineStatus{PowerState: v1alpha1.PowerOff, Target: v1alpha1.TargetOff,
			LastTransitionTime: &metav1.Time{Time: h.now}}
		if name == "big" {
			m.Status.PoweredOffAt = &metav1.Time{Time: h.now} // the scaler turned this one off
			m.Status.MeasuredWatts = "56"                     // its last reading while on
		}
		_ = h.c.Status().Update(context.Background(), m)
	}

	h.run() // first run: no interval yet, but the first report goes out
	for range 4 {
		h.now = h.now.Add(15 * time.Second)
		h.run()
	}
	var p2 v1alpha1.PowerPolicy
	h.get(&p2, "default")
	sv := p2.Status.Savings
	// Only "big": (56 - 2) W for 60 s; "found-off" wasn't powered off by us.
	wantKWh := 54.0 * (60.0 / 3600) / 1000
	if kwh, _ := strconv.ParseFloat(sv.EnergyKWh, 64); math.Abs(kwh-wantKWh) > 0.0005 {
		t.Fatalf("energy saved %s kWh, want %.4f", sv.EnergyKWh, wantKWh)
	}
	if cost, _ := strconv.ParseFloat(sv.Cost, 64); math.Abs(cost-wantKWh*0.30) > 0.00005 {
		t.Fatalf("cost saved %s, want %.6f", sv.Cost, wantKWh*0.30)
	}
	if sv.InstallID == "" || sv.Reports != 1 || len(got) != 1 {
		t.Fatalf("one report per day expected: installID=%q reports=%d received=%d", sv.InstallID, sv.Reports, len(got))
	}
	rep := got[0]
	if rep.InstallID != sv.InstallID || rep.Report != 1 || rep.Machines != "2-3" || rep.Day == "" {
		t.Fatalf("report %+v", rep)
	}

	// A day later the next report carries the same ID and higher cumulative totals.
	h.now = h.now.Add(25 * time.Hour)
	h.run()
	if len(got) != 2 || got[1].InstallID != rep.InstallID || got[1].Report != 2 || got[1].EnergySavedKWh < rep.EnergySavedKWh {
		t.Fatalf("second report %+v", got)
	}
	// Nothing identifying is ever sent.
	raw, _ := json.Marshal(got[1])
	for _, leak := range []string{"big", "found-off", "10.0.0.9", "0.30"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("report leaks %q: %s", leak, raw)
		}
	}
}

func TestTelemetryOffByDefault(t *testing.T) {
	calls := 0
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer collector.Close()
	p := policy(false)
	p.Spec.Telemetry.Endpoint = collector.URL // endpoint set, but not enabled
	h := newHarness(t, p, node("a", true, "4", "8Gi", nil))
	h.run()
	var got v1alpha1.PowerPolicy
	h.get(&got, "default")
	if calls != 0 || got.Status.Savings.InstallID != "" {
		t.Fatalf("telemetry must be opt-in: calls=%d id=%q", calls, got.Status.Savings.InstallID)
	}
}

// A fresh install with no Machines still says which workers could sleep.
func TestSuggestionsForNodesWithoutMachines(t *testing.T) {
	cp := node("cp", true, "8", "16Gi", map[string]string{"node-role.kubernetes.io/control-plane": ""})
	busy := node("busy", true, "4", "8Gi", nil)
	idle := node("idle", true, "4", "8Gi", nil)
	big := pod("db", "busy", "ReplicaSet")
	big.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("3")
	h := newHarness(t, policy(true), cp, busy, idle, big, pod("web", "idle", "ReplicaSet"))
	h.run()
	h.now = h.now.Add(16 * time.Minute) // past the default unneededTime
	h.run()
	var p v1alpha1.PowerPolicy
	h.get(&p, "default")
	if len(p.Status.Suggestions) != 1 || p.Status.Suggestions[0].Machine != "idle" || p.Status.Suggestions[0].Target != v1alpha1.TargetOff {
		t.Fatalf("want a sleep suggestion for idle only, got %+v", p.Status.Suggestions)
	}
	if len(p.Status.LastActions) != 0 || h.gotCalls() != "" || PowerTaintEffect(h.n("idle")) != "" {
		t.Fatal("suggestions must never act")
	}
	h.run() // still suggested on the next loop
	h.get(&p, "default")
	if len(p.Status.Suggestions) != 1 {
		t.Fatalf("suggestion should persist: %+v", p.Status.Suggestions)
	}
}
