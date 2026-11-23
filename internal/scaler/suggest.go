package scaler

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/planner"
)

// suggest runs a second, report-only planning pass in which worker nodes that
// have no Machine are treated as if they could be switched. Whatever it would
// power off (or wake) for those nodes becomes status.suggestions, so a fresh
// install shows which nodes could sleep before any Machine is written. It
// never affects the real plan: Machines are planned without it.
func (s *Scaler) suggest(ctx context.Context, in planner.Input) []v1alpha1.PlannedAction {
	virtual := map[string]bool{}
	nodes := make([]*planner.Node, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		if n.Managed || n.Protected || n.Node == nil {
			nodes = append(nodes, n)
			continue
		}
		v := *n
		v.Managed, v.CanPowerOn, v.CanPowerOff = true, true, true
		v.Profile = planner.Profile{IdleWatts: planner.EstimateIdleWatts(n.Node.Status.Allocatable),
			WattsPerCore: planner.DefaultWattsPerCore, BootTime: planner.DefaultBootTime}
		v.Cached = Snapshot(n.Node, n.Pods, in.Now)
		v.State, v.Target = v1alpha1.PowerOn, v1alpha1.TargetOn
		if !n.Ready {
			v.State, v.Target = v1alpha1.PowerOff, v1alpha1.TargetOff
		}
		s.mu.Lock()
		if t, ok := s.suggestUnneeded[n.Name]; ok {
			v.UnneededSince = &t
		}
		s.mu.Unlock()
		virtual[n.Name] = true
		nodes = append(nodes, &v)
	}
	if len(virtual) == 0 {
		return nil
	}
	sin := in
	sin.Nodes = nodes
	sin.Settings.MaxConcurrentDown = len(nodes) // list every node that could sleep together
	sin.Settings.MaxConcurrentUp = len(nodes)
	sin.LastScaleUp = time.Time{}
	res := planner.Plan(sin)

	s.mu.Lock()
	s.suggestUnneeded = map[string]time.Time{}
	for name, since := range res.Unneeded {
		if virtual[name] {
			s.suggestUnneeded[name] = since
		}
	}
	for _, a := range res.Actions { // a node suggested for sleep stays "unneeded" for display
		if virtual[a.Node] && a.Target == v1alpha1.TargetOff {
			s.suggestUnneeded[a.Node] = in.Now.Add(-sin.Settings.UnneededTime)
		}
	}
	s.mu.Unlock()

	var out []v1alpha1.PlannedAction
	for _, a := range res.Actions {
		if virtual[a.Node] {
			out = append(out, v1alpha1.PlannedAction{Machine: a.Node, Target: a.Target,
				Reason: a.Reason + " (no Machine yet: watts estimated from node size)"})
		}
	}
	if len(out) > 0 {
		log.FromContext(ctx).V(1).Info("suggestions for nodes without a Machine", "count", len(out))
	}
	return out
}
