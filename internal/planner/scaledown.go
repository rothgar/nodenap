package planner

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// IsDaemonSetPod reports whether a DaemonSet controls the pod.
func IsDaemonSetPod(pod *corev1.Pod) bool {
	for _, o := range pod.OwnerReferences {
		if o.Kind == "DaemonSet" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// IsMirrorPod reports whether the pod is a static pod mirror.
func IsMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

// MovablePods are the pods that must be rescheduled before the node goes
// off (everything except DaemonSet and static pods).
func MovablePods(pods []*corev1.Pod) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range pods {
		if !IsDaemonSetPod(p) && !IsMirrorPod(p) {
			out = append(out, p)
		}
	}
	return out
}

// blocker returns why a pod prevents its node from being powered off.
func blocker(pod *corev1.Pod) string {
	if pod.Annotations[v1alpha1.SafeToEvictAnnotation] == "false" {
		return "pod " + podKey(pod) + " is annotated safe-to-evict=false"
	}
	if pod.Annotations[v1alpha1.SafeToEvictAnnotation] == "true" {
		return ""
	}
	if g := groupOf(pod); g != "" {
		return "pod " + podKey(pod) + " is part of group " + g + "; moving it would restart the whole group"
	}
	hasController := false
	for _, o := range pod.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			hasController = true
		}
	}
	if !hasController {
		return "pod " + podKey(pod) + " has no controller and would not be recreated"
	}
	return ""
}

// mustMove are the pods that need a new home before n can go off:
// movable pods that aren't ignored.
func (p *run) mustMove(n *Node) []*corev1.Pod {
	var out []*corev1.Pod
	for _, pod := range MovablePods(n.Pods) {
		if !p.in.ignored(pod) {
			out = append(out, pod)
		}
	}
	return out
}

// utilization is max(cpu, memory) requested/allocatable for pods.
func utilization(n *Node, pods []*corev1.Pod) float64 {
	alloc := n.Node.Status.Allocatable
	var cpuReq, memReq int64
	for _, pod := range pods {
		r := PodRequests(pod)
		c, m := r[corev1.ResourceCPU], r[corev1.ResourceMemory]
		cpuReq += c.MilliValue()
		memReq += m.Value()
	}
	ac, am := alloc[corev1.ResourceCPU], alloc[corev1.ResourceMemory]
	u := 0.0
	if ac.MilliValue() > 0 {
		u = max(u, float64(cpuReq)/float64(ac.MilliValue()))
	}
	if am.Value() > 0 {
		u = max(u, float64(memReq)/float64(am.Value()))
	}
	return u
}

// currentWatts is the measured draw, or the profile estimate from requests.
func currentWatts(n *Node) float64 {
	if n.MeasuredWatts != nil {
		return *n.MeasuredWatts
	}
	cores := 0.0
	for _, pod := range n.Pods {
		cores += cpuCores(PodRequests(pod))
	}
	return n.Profile.Estimate(cores)
}

// wattsPerCore ranks inefficiency: power drawn per allocatable core.
func wattsPerCore(n *Node) float64 {
	c := n.Node.Status.Allocatable[corev1.ResourceCPU]
	cores := float64(c.MilliValue()) / 1000
	if cores <= 0 {
		return currentWatts(n)
	}
	return currentWatts(n) / cores
}

// placeElsewhere simulates moving pods onto views (mutating them on success).
func placeElsewhere(pods []*corev1.Pod, views map[string]*NodeView, exclude map[string]bool) (map[string]*NodeView, error) {
	trial := map[string]*NodeView{}
	for name, v := range views {
		if !exclude[name] {
			trial[name] = v.Clone()
		}
	}
	sorted := append([]*corev1.Pod(nil), pods...)
	sortPodsLargestFirst(sorted)
	names := sortedKeys(trial)
	for _, pod := range sorted {
		placed := false
		// Best fit: the node with the least free CPU that still fits keeps
		// large holes open on other machines.
		sort.SliceStable(names, func(i, j int) bool {
			a, b := trial[names[i]].Free[corev1.ResourceCPU], trial[names[j]].Free[corev1.ResourceCPU]
			return a.Cmp(b) < 0
		})
		for _, name := range names {
			if v := trial[name]; v.Fits(pod) {
				v.Add(pod)
				placed = true
				break
			}
		}
		if !placed {
			return nil, fmt.Errorf("pod %s fits on no other node", podKey(pod))
		}
	}
	return trial, nil
}

type downCandidate struct {
	n       *Node
	savings float64
	wpc     float64
}

func (p *run) scaleDown() {
	s := p.in.Settings
	now := p.in.Now
	if s.ScaleDownDisabled {
		return
	}
	if p.in.Upgrade != "" {
		p.note("no power-offs during the upgrade (%s)", p.in.Upgrade)
		return
	}

	// Track which nodes are unneeded even when we won't act this round, so
	// the unneeded timer keeps running.
	var candidates []downCandidate
	for _, n := range p.in.Nodes {
		if !n.Managed || !isOn(n) || n.Node == nil || p.off[n.Name] || p.waking[n.Name] {
			continue
		}
		if n.Protected || !n.CanPowerOff || !n.CanPowerOn || n.Override == v1alpha1.TargetOn {
			continue
		}
		if r := p.upgradeHold(n); r != "" {
			p.note("%s: %s", n.Name, r)
			continue
		}
		move := p.mustMove(n)
		if u := utilization(n, move); u >= s.UtilizationThreshold {
			continue
		}
		blocked := ""
		for _, pod := range move {
			if b := blocker(pod); b != "" {
				blocked = b
				break
			}
		}
		if blocked != "" {
			p.note("%s: %s", n.Name, blocked)
			continue
		}
		if _, err := placeElsewhere(move, p.live, map[string]bool{n.Name: true}); err != nil {
			continue
		}
		saved := p.in.Energy.SavingsToRemovePerHour(currentWatts(n) - n.Profile.OffWatts)
		if saved < s.MinSavingsPerHour {
			p.note("%s: keeping on, powering off would save only %.4f/h", n.Name, saved)
			continue
		}
		since := now
		if n.UnneededSince != nil {
			since = *n.UnneededSince
		}
		p.res.Unneeded[n.Name] = since
		if now.Sub(since) >= s.UnneededTime {
			candidates = append(candidates, downCandidate{n: n, savings: saved, wpc: wattsPerCore(n)})
		}
	}

	if len(candidates) == 0 {
		return
	}
	if len(p.waking) > 0 || p.inFlightUp > 0 {
		p.note("not scaling down while machines are being woken")
		return
	}
	if !p.in.LastScaleUp.IsZero() && now.Sub(p.in.LastScaleUp) < s.DelayAfterScaleUp {
		p.note("not scaling down within %s of the last scale-up", s.DelayAfterScaleUp)
		return
	}

	// Least efficient first, then biggest saving, then lowest priority.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.wpc != b.wpc {
			return a.wpc > b.wpc
		}
		if a.savings != b.savings {
			return a.savings > b.savings
		}
		if a.n.Priority != b.n.Priority {
			return a.n.Priority < b.n.Priority
		}
		return a.n.Name < b.n.Name
	})

	readyOn := 0
	for _, n := range p.in.Nodes {
		if isOn(n) && !p.off[n.Name] && n.Target != v1alpha1.TargetOff {
			readyOn++
		}
	}

	removed := map[string]bool{}
	for _, c := range candidates {
		if s.MaxConcurrentDown > 0 && p.inFlightDown+len(p.off) >= s.MaxConcurrentDown {
			break
		}
		if readyOn-1 < s.MinNodesOn {
			p.note("minNodesOn=%d reached", s.MinNodesOn)
			break
		}
		removed[c.n.Name] = true
		trial, err := placeElsewhere(p.mustMove(c.n), p.live, removed)
		if err != nil {
			delete(removed, c.n.Name)
			continue
		}
		// Check headroom without this node.
		saved := p.live
		p.live = trial
		cpu, mem := p.freeCapacity()
		short := p.headroomShort(cpu, mem)
		if short {
			p.live = saved
			delete(removed, c.n.Name)
			continue
		}
		readyOn--
		p.act(c.n, v1alpha1.TargetOff, fmt.Sprintf("unneeded for %s at %.0f%% utilization; saves %.0fW (%.4f/h)",
			now.Sub(p.res.Unneeded[c.n.Name]).Round(time.Second), utilization(c.n, p.mustMove(c.n))*100,
			currentWatts(c.n)-c.n.Profile.OffWatts, c.savings))
		delete(p.res.Unneeded, c.n.Name)
	}
}

// estimate fills the fleet power/cost summary.
func (p *run) estimate() {
	var watts float64
	for _, n := range p.in.Nodes {
		if !n.Managed {
			if n.Ready {
				p.res.NodesOn++
			}
			continue
		}
		switch n.State {
		case v1alpha1.PowerOff:
			p.res.NodesOff++
			watts += n.Profile.OffWatts
		default:
			if n.Ready {
				p.res.NodesOn++
			}
			watts += currentWattsSafe(n)
		}
	}
	p.res.EstimatedWatts = watts
	// What the fleet costs now = what turning all of it off would save.
	p.res.EstimatedCostPerHour = p.in.Energy.SavingsToRemovePerHour(watts)
}

func currentWattsSafe(n *Node) float64 {
	if n.MeasuredWatts != nil {
		return *n.MeasuredWatts
	}
	if n.Node == nil {
		return n.Profile.IdleWatts
	}
	return currentWatts(n)
}
