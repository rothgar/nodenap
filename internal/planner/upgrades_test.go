package planner

import (
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// setVersions gives every node old versions, then the named ones new ones.
func setVersions(nodes []*Node, upgraded ...string) {
	for _, n := range nodes {
		n.Node.Status.NodeInfo.OSImage = "Talos (v1.14.5)"
		n.Node.Status.NodeInfo.KubeletVersion = "v1.37.2"
	}
	for _, name := range upgraded {
		byName(nodes, name).Node.Status.NodeInfo.OSImage = "Talos (v1.15.0)"
	}
}

func TestUpgradeWakesMachinesThatAreBehind(t *testing.T) {
	nodes := build(fleet())
	setVersions(nodes, "pi4-1") // one running node already upgraded
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	var woken []string
	for _, a := range r.Actions {
		if a.Target == v1alpha1.TargetOn && strings.HasPrefix(a.Reason, "upgrade: Talos v1.14.5 → v1.15.0") {
			if a.UpgradeFrom != "Talos (v1.14.5), kubelet v1.37.2" {
				t.Fatalf("UpgradeFrom = %q", a.UpgradeFrom)
			}
			woken = append(woken, a.Node)
		}
	}
	if strings.Join(woken, ",") != "pi4-2,spark,qb2" {
		t.Fatalf("want every off machine woken, got %v (%v)", woken, r.Actions)
	}
}

func TestNoUpgradeWakeWhenVersionsMatch(t *testing.T) {
	nodes := build(fleet())
	setVersions(nodes)
	if r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()}); len(r.Actions) != 0 {
		t.Fatalf("want nothing, got %v", r.Actions)
	}
}

func TestKubeletUpgradeWakes(t *testing.T) {
	nodes := build(fleet())
	setVersions(nodes)
	byName(nodes, "mini").Node.Status.NodeInfo.KubeletVersion = "v1.38.0"
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if targets(r)["spark"] != v1alpha1.TargetOn || !strings.Contains(r.Actions[0].Reason, "kubelet v1.37.2 → v1.38.0") {
		t.Fatalf("got %v", r.Actions)
	}
}

func TestUpgradeWakesOnlyOncePerVersion(t *testing.T) {
	nodes := build(fleet())
	setVersions(nodes, "pi4-1")
	for _, n := range nodes {
		n.UpgradeFrom = "Talos (v1.14.5), kubelet v1.37.2" // woken before, didn't upgrade
	}
	if r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()}); len(r.Actions) != 0 {
		t.Fatalf("want no second wake, got %v", r.Actions)
	}
}

func TestUpgradeHoldsMachineOnUntilCaughtUp(t *testing.T) {
	nodes := scaleDownFleet()
	setVersions(nodes, "pi4-1", "mini")
	for _, n := range nodes {
		n.UnneededSince = ptr.To(now.Add(-time.Hour))
	}
	qb2 := byName(nodes, "qb2")
	qb2.UpgradeFrom, qb2.UpgradeAt = "Talos (v1.14.5), kubelet v1.37.2", now.Add(-10*time.Minute)
	r := Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if targets(r)["qb2"] != "" {
		t.Fatalf("qb2 should stay on until it's upgraded: %v", r.Actions)
	}

	// Once upgraded, it's powered off as usual.
	qb2.Node.Status.NodeInfo.OSImage = "Talos (v1.15.0)"
	r = Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if targets(r)["qb2"] != v1alpha1.TargetOff {
		t.Fatalf("upgraded qb2 should go off: %v %v", r.Actions, r.Notes)
	}

	// A machine that never upgrades is let go after upgradeHoldTime.
	qb2.Node.Status.NodeInfo.OSImage = "Talos (v1.14.5)"
	qb2.UpgradeAt = now.Add(-2 * upgradeHoldTime)
	r = Plan(Input{Now: now, Nodes: nodes, Energy: grid(0.30), Settings: settings()})
	if targets(r)["qb2"] != v1alpha1.TargetOff {
		t.Fatalf("hold should expire: %v %v", r.Actions, r.Notes)
	}
}

func TestVersionsOf(t *testing.T) {
	for _, tc := range []struct{ osImage, name, os string }{
		{"Talos Enterprise (v1.14.5)", "Talos Enterprise", "1.14.5"},
		{"Ubuntu 24.04.1 LTS", "Ubuntu", "24.4.1"},
		{"Flatcar Container Linux by Kinvolk 4152.2.0 (Oklo)", "Flatcar Container Linux by Kinvolk", "4152.2.0"},
	} {
		n := build(fleet()[:1])[0].Node
		n.Status.NodeInfo.OSImage, n.Status.NodeInfo.KubeletVersion = tc.osImage, "v1.31.4+k3s1"
		v := versionsOf(n)
		if v.osName != tc.name || v.os.String() != tc.os || v.kubelet.String() != "1.31.4" {
			t.Fatalf("%q: got %q %v %v", tc.osImage, v.osName, v.os, v.kubelet)
		}
	}
}
