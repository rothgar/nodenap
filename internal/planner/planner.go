package planner

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// Plan computes actions for the input.
func Plan(in Input) Result {
	p := &run{in: in, res: Result{Unneeded: map[string]time.Time{}}}
	p.index()
	p.overrides()
	p.clusterUpgrade()
	p.upgrades()
	p.scaleUp()
	p.headroom()
	p.scaleDown()
	p.estimate()
	return p.res
}

type run struct {
	in  Input
	res Result

	byName map[string]*Node
	// views of schedulable, Ready, on nodes with their current pods.
	live map[string]*NodeView
	// views of nodes booting (from cache) that pending pods will land on.
	booting                  map[string]*NodeView
	waking                   map[string]bool // decided this run
	off                      map[string]bool // decided this run
	inFlightUp, inFlightDown int
}

func isOn(n *Node) bool {
	if !n.Managed {
		return n.Ready
	}
	return n.State == v1alpha1.PowerOn && n.Ready
}

func (p *run) index() {
	p.byName = map[string]*Node{}
	p.live = map[string]*NodeView{}
	p.booting = map[string]*NodeView{}
	p.waking = map[string]bool{}
	p.off = map[string]bool{}
	for _, n := range p.in.Nodes {
		p.byName[n.Name] = n
		switch {
		case n.Managed && (n.State == v1alpha1.PowerBooting || (n.Target == v1alpha1.TargetOn && n.State != v1alpha1.PowerOn)):
			p.inFlightUp++
			if n.Cached != nil {
				p.booting[n.Name] = NewCachedNodeView(n.Name, n.Cached)
			}
		case n.Managed && (n.State == v1alpha1.PowerDraining || n.State == v1alpha1.PowerShuttingDown):
			p.inFlightDown++
		case isOn(n) && n.Node != nil && n.Target != v1alpha1.TargetOff:
			p.live[n.Name] = NewNodeView(n.Node, n.Pods)
		}
	}
}

func (p *run) act(n *Node, target v1alpha1.Target, reason string) {
	p.res.Actions = append(p.res.Actions, Action{Node: n.Name, Target: target, Reason: reason})
	if target == v1alpha1.TargetOn {
		p.waking[n.Name] = true
	} else {
		p.off[n.Name] = true
	}
}

func (p *run) note(format string, args ...any) {
	p.res.Notes = append(p.res.Notes, fmt.Sprintf(format, args...))
}

func (p *run) overrides() {
	for _, n := range p.in.Nodes {
		if !n.Managed || n.Override == "" {
			continue
		}
		switch {
		case n.Override == v1alpha1.TargetOn && n.Target != v1alpha1.TargetOn:
			p.act(n, v1alpha1.TargetOn, "override annotation")
		case n.Override == v1alpha1.TargetOff && n.Target != v1alpha1.TargetOff:
			if n.Protected {
				p.note("ignoring override off for %s: %s", n.Name, n.ProtectedReason)
				continue
			}
			p.act(n, v1alpha1.TargetOff, "override annotation")
			delete(p.live, n.Name)
		}
	}
}

func podKey(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }

func cpuCores(rl corev1.ResourceList) float64 {
	q := rl[corev1.ResourceCPU]
	return float64(q.MilliValue()) / 1000
}

// wakeCandidates are managed, off machines we know enough about to wake.
func (p *run) wakeCandidates() []*Node {
	var out []*Node
	for _, n := range p.in.Nodes {
		if !n.Managed || !n.CanPowerOn || n.Cached == nil || p.waking[n.Name] || n.Override == v1alpha1.TargetOff {
			continue
		}
		if n.Target == v1alpha1.TargetOn {
			continue // already on or on its way
		}
		if n.State == v1alpha1.PowerOff || n.State == v1alpha1.PowerFailed {
			out = append(out, n)
		}
	}
	return out
}

// estimateFor is the expected draw if node runs pods.
func estimateFor(n *Node, pods []*corev1.Pod) float64 {
	cores := 0.0
	for _, pod := range pods {
		cores += cpuCores(PodRequests(pod))
	}
	if n.Cached != nil {
		cores += cpuCores(n.Cached.DaemonSetRequests)
	}
	return n.Profile.Estimate(cores)
}

func sortPodsLargestFirst(pods []*corev1.Pod) {
	sort.SliceStable(pods, func(i, j int) bool {
		a, b := PodRequests(pods[i]), PodRequests(pods[j])
		ac, bc := a[corev1.ResourceCPU], b[corev1.ResourceCPU]
		if c := ac.Cmp(bc); c != 0 {
			return c > 0
		}
		am, bm := a[corev1.ResourceMemory], b[corev1.ResourceMemory]
		return am.Cmp(bm) > 0
	})
}

// scaleUp wakes the cheapest set of machines that can run the pending pods.
func (p *run) scaleUp() {
	var pending []*corev1.Pod
	for _, pod := range p.in.PendingPods {
		if !p.in.ignored(pod) {
			pending = append(pending, pod)
		}
	}
	sortPodsLargestFirst(pending)

	// Pods that will land on machines already booting don't need more.
	var remaining []*corev1.Pod
	for _, pod := range pending {
		placed := false
		for _, name := range sortedKeys(p.booting) {
			if v := p.booting[name]; v.Fits(pod) {
				v.Add(pod)
				placed = true
				break
			}
		}
		if !placed {
			remaining = append(remaining, pod)
		}
	}

	maxUp := p.in.Settings.MaxConcurrentUp
	candidates := p.wakeCandidates()
	remaining = p.dropPartialGroups(remaining, candidates)
	for len(remaining) > 0 {
		if maxUp > 0 && p.inFlightUp+len(p.waking) >= maxUp {
			p.note("scale-up limited by maxConcurrent=%d; %d pods still waiting", maxUp, len(remaining))
			break
		}
		var best *Node
		var bestPods []*corev1.Pod
		bestScore := math.Inf(1)
		for _, c := range candidates {
			if p.waking[c.Name] {
				continue
			}
			v := NewCachedNodeView(c.Name, c.Cached)
			var placed []*corev1.Pod
			for _, pod := range remaining {
				if v.Fits(pod) {
					v.Add(pod)
					placed = append(placed, pod)
				}
			}
			if len(placed) == 0 {
				continue
			}
			cost := p.in.Energy.CostToAddPerHour(estimateFor(c, placed) - c.Profile.OffWatts)
			// Cost per pod served; a tiny epsilon makes free power still
			// prefer lower-wattage machines.
			score := (cost + estimateFor(c, placed)*1e-9) / float64(len(placed))
			if better(score, c, bestScore, best) {
				best, bestPods, bestScore = c, placed, score
			}
		}
		if best == nil {
			break
		}
		names := make([]string, 0, len(bestPods))
		for _, pod := range bestPods {
			names = append(names, podKey(pod))
		}
		p.act(best, v1alpha1.TargetOn, fmt.Sprintf("%d pending pod(s) fit (%s); est. %.0fW at %.3f/kWh",
			len(bestPods), truncateList(names, 3), estimateFor(best, bestPods), p.in.Energy.MarginalPrice(estimateFor(best, bestPods))))
		placed := map[*corev1.Pod]bool{}
		for _, pod := range bestPods {
			placed[pod] = true
		}
		var next []*corev1.Pod
		for _, pod := range remaining {
			if !placed[pod] {
				next = append(next, pod)
			}
		}
		remaining = next
	}

	for _, pod := range remaining {
		reason := "no powered-off machine can run it"
		if len(candidates) > 0 {
			reason = "no powered-off machine can run it: " + p.explain(pod, candidates)
		}
		p.res.Unsatisfiable = append(p.res.Unsatisfiable, podKey(pod)+": "+reason)
	}
}

// better ranks wake candidates: lower score, then faster boot, then priority.
func better(score float64, c *Node, bestScore float64, best *Node) bool {
	if best == nil {
		return true
	}
	if math.Abs(score-bestScore) > 1e-12 {
		return score < bestScore
	}
	if c.Profile.BootTime != best.Profile.BootTime {
		return c.Profile.BootTime < best.Profile.BootTime
	}
	if c.Priority != best.Priority {
		return c.Priority > best.Priority
	}
	return c.Name < best.Name
}

func (p *run) explain(pod *corev1.Pod, candidates []*Node) string {
	var reasons []string
	for _, c := range candidates {
		if err := NewCachedNodeView(c.Name, c.Cached).Check(pod); err != nil {
			reasons = append(reasons, err.Error())
		}
	}
	return truncateList(reasons, 3)
}

func truncateList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// freeCapacity sums free CPU and memory over schedulable on/booting nodes.
func (p *run) freeCapacity() (cpu, mem int64) {
	add := func(v *NodeView) {
		c, m := v.Free[corev1.ResourceCPU], v.Free[corev1.ResourceMemory]
		cpu += c.MilliValue()
		mem += m.Value()
	}
	for name, v := range p.live {
		if !p.off[name] && !v.Node.Spec.Unschedulable {
			add(v)
		}
	}
	for _, v := range p.booting {
		add(v)
	}
	return cpu, mem
}

func (p *run) headroomShort(cpu, mem int64) bool {
	s := p.in.Settings
	return (s.HeadroomCPU != nil && cpu < s.HeadroomCPU.MilliValue()) ||
		(s.HeadroomMemory != nil && mem < s.HeadroomMemory.Value())
}

// headroom wakes machines until spare capacity meets the policy.
func (p *run) headroom() {
	cpu, mem := p.freeCapacity()
	for _, name := range sortedKeys(p.waking) {
		if n := p.byName[name]; n.Cached != nil {
			v := NewCachedNodeView(name, n.Cached)
			c, m := v.Free[corev1.ResourceCPU], v.Free[corev1.ResourceMemory]
			cpu += c.MilliValue()
			mem += m.Value()
		}
	}
	for p.headroomShort(cpu, mem) {
		var best *Node
		bestScore := math.Inf(1)
		for _, c := range p.wakeCandidates() {
			v := NewCachedNodeView(c.Name, c.Cached)
			cq := v.Free[corev1.ResourceCPU]
			cores := float64(cq.MilliValue()) / 1000
			if cores <= 0 {
				continue
			}
			score := (p.in.Energy.CostToAddPerHour(c.Profile.IdleWatts-c.Profile.OffWatts) + c.Profile.IdleWatts*1e-9) / cores
			if better(score, c, bestScore, best) {
				best, bestScore = c, score
			}
		}
		if best == nil {
			p.note("headroom not met and no machine left to wake")
			return
		}
		v := NewCachedNodeView(best.Name, best.Cached)
		c, m := v.Free[corev1.ResourceCPU], v.Free[corev1.ResourceMemory]
		cpu += c.MilliValue()
		mem += m.Value()
		p.act(best, v1alpha1.TargetOn, "keeping headroom")
	}
}
