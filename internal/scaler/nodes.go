package scaler

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/planner"
)

// NodeReady reports the Ready condition.
func NodeReady(n *corev1.Node) bool {
	if n == nil {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// NotReadySince returns when the node last stopped being Ready.
func NotReadySince(n *corev1.Node) time.Time {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

func isControlPlane(n *corev1.Node) bool {
	_, cp := n.Labels["node-role.kubernetes.io/control-plane"]
	_, master := n.Labels["node-role.kubernetes.io/master"]
	return cp || master
}

func powerTaint(n *corev1.Node) *corev1.Taint {
	for i := range n.Spec.Taints {
		if n.Spec.Taints[i].Key == v1alpha1.TaintKey {
			return &n.Spec.Taints[i]
		}
	}
	return nil
}

// PowerTaintEffect returns our taint's effect on the node, or "".
func PowerTaintEffect(n *corev1.Node) corev1.TaintEffect {
	if t := powerTaint(n); t != nil {
		return t.Effect
	}
	return ""
}

// cordonedByUs is the taint value recording that the scaler (not a person)
// cordoned the node, so it only ever uncordons what it cordoned.
const cordonedByUs = "cordoned"

// setTaint adds our taint with effect (and cordons), or removes it with
// effect "" (uncordoning only if we cordoned). Draining uses NoSchedule;
// powered-off machines get NoExecute, because pods that tolerate every
// NoSchedule taint would otherwise be scheduled onto the dead node.
func setTaint(ctx context.Context, c client.Client, node *corev1.Node, effect corev1.TaintEffect) error {
	t := powerTaint(node)
	switch {
	case effect == "" && t == nil:
		return nil
	case effect != "" && t != nil && t.Effect == effect && node.Spec.Unschedulable:
		return nil
	}
	orig := node.DeepCopy()
	if effect == "" {
		var keep []corev1.Taint
		for _, tt := range node.Spec.Taints {
			if tt.Key != v1alpha1.TaintKey {
				keep = append(keep, tt)
			}
		}
		node.Spec.Taints = keep
		if t.Value == cordonedByUs {
			node.Spec.Unschedulable = false
		}
	} else {
		if t == nil {
			value := ""
			if !node.Spec.Unschedulable {
				value = cordonedByUs
			}
			node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: v1alpha1.TaintKey, Value: value, Effect: effect,
				TimeAdded: &metav1.Time{Time: time.Now()}})
		} else {
			if !node.Spec.Unschedulable {
				t.Value = cordonedByUs // someone uncordoned it; we re-cordon, so it's ours
			}
			t.Effect = effect
		}
		node.Spec.Unschedulable = true
	}
	return c.Patch(ctx, node, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// transientTaints come from node conditions and must not be cached as
// properties of the machine.
var transientTaints = map[string]bool{
	v1alpha1.TaintKey:                           true,
	corev1.TaintNodeNotReady:                    true,
	corev1.TaintNodeUnreachable:                 true,
	corev1.TaintNodeUnschedulable:               true,
	corev1.TaintNodeMemoryPressure:              true,
	corev1.TaintNodeDiskPressure:                true,
	corev1.TaintNodePIDPressure:                 true,
	corev1.TaintNodeNetworkUnavailable:          true,
	"node.kubernetes.io/out-of-service":         true,
	"node.cloudprovider.kubernetes.io/shutdown": true,
}

// Snapshot captures what the planner needs while the machine is off.
func Snapshot(node *corev1.Node, pods []*corev1.Pod, now time.Time) *v1alpha1.CachedNode {
	c := &v1alpha1.CachedNode{Labels: maps.Clone(node.Labels), Allocatable: node.Status.Allocatable.DeepCopy(),
		CapturedAt: metav1.Time{Time: now}}
	for _, t := range node.Spec.Taints {
		if !transientTaints[t.Key] {
			t.TimeAdded = nil
			c.Taints = append(c.Taints, t)
		}
	}
	ds := corev1.ResourceList{}
	for _, p := range pods {
		if planner.IsDaemonSetPod(p) {
			for res, q := range planner.PodRequests(p) {
				cur := ds[res]
				cur.Add(q)
				ds[res] = cur
			}
		}
	}
	if len(ds) > 0 {
		c.DaemonSetRequests = ds
	}
	return c
}

func snapshotChanged(a, b *v1alpha1.CachedNode) bool {
	if a == nil || b == nil {
		return a != b
	}
	return !reflect.DeepEqual(a.Labels, b.Labels) ||
		!equality.Semantic.DeepEqual(a.Taints, b.Taints) ||
		!equality.Semantic.DeepEqual(a.Allocatable, b.Allocatable) ||
		!equality.Semantic.DeepEqual(a.DaemonSetRequests, b.DaemonSetRequests)
}

// ignoreMatcher builds the predicate for spec.ignorePods plus the
// nodenap.rothgar.github.io/ignore annotation.
func ignoreMatcher(matchers []v1alpha1.PodMatcher) (func(*corev1.Pod) bool, error) {
	type compiled struct {
		ns  string
		sel labels.Selector
	}
	var cs []compiled
	for i, m := range matchers {
		if m.Namespace == "" && m.Selector == nil {
			return nil, fmt.Errorf("ignorePods[%d]: set namespace and/or selector", i)
		}
		sel := labels.Everything()
		if m.Selector != nil {
			s, err := metav1.LabelSelectorAsSelector(m.Selector)
			if err != nil {
				return nil, fmt.Errorf("ignorePods[%d]: %w", i, err)
			}
			sel = s
		}
		cs = append(cs, compiled{m.Namespace, sel})
	}
	return func(p *corev1.Pod) bool {
		if p.Annotations[v1alpha1.IgnorePodAnnotation] == "true" {
			return true
		}
		for _, c := range cs {
			if (c.ns == "" || c.ns == p.Namespace) && c.sel.Matches(labels.Set(p.Labels)) {
				return true
			}
		}
		return false
	}, nil
}

func unschedulableSince(p *corev1.Pod) (time.Time, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}
