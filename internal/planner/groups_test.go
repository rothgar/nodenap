package planner

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// lws makes a pod a member of a two-pod LeaderWorkerSet group pinned to host.
func lws(group, host string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Labels = map[string]string{"leaderworkerset.sigs.k8s.io/group-key": group}
		p.Annotations = map[string]string{"leaderworkerset.sigs.k8s.io/size": "2"}
		p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": host}
	}
}

func TestGroupWakesAllItsMachines(t *testing.T) {
	nodes := build(fleet())
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("leader", "1", "64Gi", lws("g1", "spark")), pod("worker", "1", "64Gi", lws("g1", "qb2"))}})
	got := targets(r)
	if len(got) != 2 || got["spark"] != v1alpha1.TargetOn || got["qb2"] != v1alpha1.TargetOn {
		t.Fatalf("want spark and qb2 woken together, got %v (unsat %v)", got, r.Unsatisfiable)
	}
}

func TestGroupThatCannotFullyFitWakesNothing(t *testing.T) {
	nodes := build(fleet())
	// The worker wants a machine that doesn't exist, so the leader alone
	// would sit on spark waiting forever.
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings(),
		PendingPods: []*corev1.Pod{pod("leader", "1", "64Gi", lws("g1", "spark")), pod("worker", "1", "64Gi", lws("g1", "nope"))}})
	if got := targets(r); len(got) != 0 {
		t.Fatalf("want nothing woken for a partial group, got %v", got)
	}
	if len(r.Unsatisfiable) != 1 || !strings.Contains(r.Unsatisfiable[0], "only 1 of 2") {
		t.Fatalf("want the group reported, got %v", r.Unsatisfiable)
	}
}

func TestGroupMemberBlocksPowerOff(t *testing.T) {
	nodes := scaleDownFleet()
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	// Small enough to move, but moving it would restart its group.
	p := pod("worker", "100m", "100Mi", lws("g1", "qb2"))
	p.Spec.NodeSelector = nil
	byName(nodes, "qb2").Pods = []*corev1.Pod{p}
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if targets(r)["qb2"] != "" {
		t.Fatalf("group member should keep qb2 on: %v", r.Actions)
	}
}

func TestSinglePodGroupMovesLikeAnyPod(t *testing.T) {
	p := pod("solo", "100m", "100Mi", lws("g1", "qb2"))
	p.Annotations["leaderworkerset.sigs.k8s.io/size"] = "1"
	if g := groupOf(p); g != "" {
		t.Fatalf("size-1 group should not count as a group, got %q", g)
	}
}
