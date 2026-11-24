package planner

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Some pods only work together, such as a LeaderWorkerSet group spread over
// several machines. Their controllers label every member, so nodenap finds
// groups without configuration. It wakes machines for a group only when every
// pending member can be placed, and doesn't drain a node running part of a
// group, because evicting one member restarts the whole group.

// groupLabels name a pod's group; the first one set wins.
var groupLabels = []string{
	"leaderworkerset.sigs.k8s.io/group-key", // LeaderWorkerSet
	"scheduling.x-k8s.io/pod-group",         // scheduler-plugins coscheduling
}

// groupOf returns the pod's group, or "" for a pod that stands alone.
func groupOf(pod *corev1.Pod) string {
	if pod.Annotations["leaderworkerset.sigs.k8s.io/size"] == "1" {
		return "" // a one-pod LeaderWorkerSet group moves like any pod
	}
	for _, l := range groupLabels {
		if v := pod.Labels[l]; v != "" {
			return pod.Namespace + "/" + v
		}
	}
	return ""
}

// dropPartialGroups removes pending group members whose group can't be placed
// in full on the booting and powered-off machines, so no machine is woken for
// a group that couldn't start, and reports why.
func (p *run) dropPartialGroups(pending []*corev1.Pod, candidates []*Node) []*corev1.Pod {
	groups := map[string][]*corev1.Pod{}
	for _, pod := range pending {
		if g := groupOf(pod); g != "" {
			groups[g] = append(groups[g], pod)
		}
	}
	drop := map[*corev1.Pod]bool{}
	for _, g := range sortedKeys(groups) {
		var views []*NodeView
		for _, name := range sortedKeys(p.booting) {
			views = append(views, p.booting[name].Clone())
		}
		for _, c := range candidates {
			views = append(views, NewCachedNodeView(c.Name, c.Cached))
		}
		members, placed := groups[g], 0
		for _, pod := range members {
			for _, v := range views {
				if v.Fits(pod) {
					v.Add(pod)
					placed++
					break
				}
			}
		}
		if placed < len(members) {
			for _, pod := range members {
				drop[pod] = true
			}
			p.res.Unsatisfiable = append(p.res.Unsatisfiable, fmt.Sprintf(
				"group %s: only %d of %d pending pods can be placed, so no machine is woken for it", g, placed, len(members)))
		}
	}
	if len(drop) == 0 {
		return pending
	}
	var out []*corev1.Pod
	for _, pod := range pending {
		if !drop[pod] {
			out = append(out, pod)
		}
	}
	return out
}
