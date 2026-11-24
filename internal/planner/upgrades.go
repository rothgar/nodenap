package planner

import (
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/version"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// A machine that's off misses upgrades rolled out to the others (Omni,
// talosctl, k3s system-upgrade-controller…). Its Node object keeps the
// versions it last reported, so when a Ready node runs a newer OS or kubelet,
// the machine is woken to catch up, and kept on for up to upgradeHoldTime
// until it does. It's woken at most once per set of old versions, so a
// machine deliberately left on an older version doesn't wake over and over.

const upgradeHoldTime = time.Hour

var versionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

type nodeVersion struct {
	osName      string // osImage without its version, e.g. "Talos"
	os, kubelet *version.Version
	key         string // what was reported, recorded in status.upgradeWake
}

func versionsOf(n *corev1.Node) nodeVersion {
	info := n.Status.NodeInfo
	v := nodeVersion{key: info.OSImage + ", kubelet " + info.KubeletVersion}
	if loc := versionRE.FindStringIndex(info.OSImage); loc != nil {
		v.os, _ = version.ParseGeneric(info.OSImage[loc[0]:loc[1]])
		v.osName = strings.TrimRight(info.OSImage[:loc[0]], " (v")
	}
	if s := versionRE.FindString(info.KubeletVersion); s != "" {
		v.kubelet, _ = version.ParseGeneric(s)
	}
	return v
}

// behind says what newer versions Ready nodes run than n's Node reports, or "".
func (p *run) behind(n *Node) string {
	mine := versionsOf(n.Node)
	var newOS, newKubelet *version.Version
	for _, o := range p.in.Nodes {
		if o == n || o.Node == nil || !o.Ready {
			continue
		}
		v := versionsOf(o.Node)
		if v.os != nil && mine.os != nil && v.osName == mine.osName && v.os.GreaterThan(mine.os) &&
			(newOS == nil || v.os.GreaterThan(newOS)) {
			newOS = v.os
		}
		if v.kubelet != nil && mine.kubelet != nil && v.kubelet.GreaterThan(mine.kubelet) &&
			(newKubelet == nil || v.kubelet.GreaterThan(newKubelet)) {
			newKubelet = v.kubelet
		}
	}
	var parts []string
	if newOS != nil {
		parts = append(parts, mine.osName+" v"+mine.os.String()+" → v"+newOS.String())
	}
	if newKubelet != nil {
		parts = append(parts, "kubelet v"+mine.kubelet.String()+" → v"+newKubelet.String())
	}
	return strings.Join(parts, ", ")
}

// upgrades wakes powered-off machines whose versions are behind.
func (p *run) upgrades() {
	for _, n := range p.in.Nodes {
		if !n.Managed || !n.CanPowerOn || n.Node == nil || n.State != v1alpha1.PowerOff ||
			n.Target == v1alpha1.TargetOn || p.waking[n.Name] || n.Override == v1alpha1.TargetOff {
			continue
		}
		why := p.behind(n)
		if why == "" {
			continue
		}
		key := versionsOf(n.Node).key
		if n.UpgradeFrom == key {
			p.note("%s: not woken again for an upgrade (%s); it was already woken once for it", n.Name, why)
			continue
		}
		if maxUp := p.in.Settings.MaxConcurrentUp; maxUp > 0 && p.inFlightUp+len(p.waking) >= maxUp {
			p.note("upgrade wake-ups limited by maxConcurrent=%d", maxUp)
			return
		}
		p.act(n, v1alpha1.TargetOn, "upgrade: "+why)
		p.res.Actions[len(p.res.Actions)-1].UpgradeFrom = key
		if n.Cached != nil {
			p.booting[n.Name] = NewCachedNodeView(n.Name, n.Cached)
		}
	}
}

// upgradeHold says why a machine woken for an upgrade stays on, or "".
func (p *run) upgradeHold(n *Node) string {
	if n.UpgradeFrom == "" || versionsOf(n.Node).key != n.UpgradeFrom || p.in.Now.Sub(n.UpgradeAt) > upgradeHoldTime {
		return ""
	}
	if why := p.behind(n); why != "" {
		return "kept on to catch up with an upgrade (" + why + ")"
	}
	return ""
}
