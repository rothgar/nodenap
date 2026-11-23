// Scheduling simulation: whether pods can schedule on nodes, including nodes
// that are powered off (using a cached snapshot of the Node).
//
// It implements the scheduler predicates that decide which machine to wake:
// resources (incl. extended resources like GPUs), nodeSelector, required node
// affinity, taints/tolerations, host ports and pod count. Inter-pod affinity
// and topology spread are not simulated.
package planner

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	resourcehelper "k8s.io/component-helpers/resource"
	schedcorev1 "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// NodeView is a node plus its remaining capacity during a simulation.
type NodeView struct {
	Node      *corev1.Node
	Free      corev1.ResourceList
	Pods      int
	MaxPods   int64
	HostPorts map[string]bool
	// IgnoreTaints are taint keys disregarded (e.g. our own power-off taint
	// when simulating a node that would be woken).
	IgnoreTaints map[string]bool
	// IgnoreUnschedulable treats a cordoned node as schedulable.
	IgnoreUnschedulable bool
}

// PodRequests returns the effective requests of a pod (init containers,
// sidecars and overhead accounted for).
func PodRequests(pod *corev1.Pod) corev1.ResourceList {
	return resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})
}

// NewNodeView builds a view of node with pods already placed on it.
func NewNodeView(node *corev1.Node, pods []*corev1.Pod) *NodeView {
	v := &NodeView{
		Node:      node,
		Free:      node.Status.Allocatable.DeepCopy(),
		HostPorts: map[string]bool{},
	}
	if v.Free == nil {
		v.Free = corev1.ResourceList{}
	}
	if q, ok := node.Status.Allocatable[corev1.ResourcePods]; ok {
		v.MaxPods = q.Value()
	} else {
		v.MaxPods = 110
	}
	for _, p := range pods {
		v.Add(p)
	}
	return v
}

// NodeFromCache synthesizes a Node object from a ManagedNode's cached
// snapshot so the standard predicates can run against it.
func NodeFromCache(name string, c *v1alpha1.CachedNode) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if c == nil {
		return n
	}
	n.Labels = c.Labels
	n.Spec.Taints = c.Taints
	n.Status.Allocatable = c.Allocatable
	return n
}

// NewCachedNodeView builds a view of a powered-off node: its cached
// allocatable minus what its DaemonSets will take once it boots.
func NewCachedNodeView(name string, c *v1alpha1.CachedNode) *NodeView {
	v := NewNodeView(NodeFromCache(name, c), nil)
	v.IgnoreTaints = map[string]bool{v1alpha1.TaintKey: true}
	v.IgnoreUnschedulable = true
	if c != nil {
		for res, q := range c.DaemonSetRequests {
			sub(v.Free, res, q)
		}
	}
	return v
}

func sub(rl corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	cur := rl[name]
	cur.Sub(q)
	rl[name] = cur
}

// Clone returns an independent copy for what-if simulation.
func (v *NodeView) Clone() *NodeView {
	c := *v
	c.Free = v.Free.DeepCopy()
	c.HostPorts = make(map[string]bool, len(v.HostPorts))
	for k := range v.HostPorts {
		c.HostPorts[k] = true
	}
	return &c
}

// Add places pod on the node, consuming its resources.
func (v *NodeView) Add(pod *corev1.Pod) {
	for res, q := range PodRequests(pod) {
		sub(v.Free, res, q)
	}
	v.Pods++
	for _, port := range hostPorts(pod) {
		v.HostPorts[port] = true
	}
}

func hostPorts(pod *corev1.Pod) []string {
	var out []string
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort > 0 {
				proto := p.Protocol
				if proto == "" {
					proto = corev1.ProtocolTCP
				}
				out = append(out, string(proto)+"/"+p.HostIP+"/"+strconv.Itoa(int(p.HostPort)))
			}
		}
	}
	return out
}

var hardTaints = func(t *corev1.Taint) bool {
	return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
}

// Check returns nil if pod fits, or an error describing why not.
func (v *NodeView) Check(pod *corev1.Pod) error {
	n := v.Node
	if n.Spec.Unschedulable && !v.IgnoreUnschedulable {
		tolerated := false
		for _, t := range pod.Spec.Tolerations {
			if t.Key == corev1.TaintNodeUnschedulable && (t.Operator == corev1.TolerationOpExists || t.Effect == "" || t.Effect == corev1.TaintEffectNoSchedule) {
				tolerated = true
			}
		}
		if !tolerated {
			return fmt.Errorf("node %s is cordoned", n.Name)
		}
	}
	if len(pod.Spec.NodeSelector) > 0 &&
		!labels.SelectorFromSet(pod.Spec.NodeSelector).Matches(labels.Set(n.Labels)) {
		return fmt.Errorf("node %s doesn't match nodeSelector", n.Name)
	}
	if ok, err := nodeaffinity.GetRequiredNodeAffinity(pod).Match(n); err != nil || !ok {
		return fmt.Errorf("node %s doesn't match required node affinity", n.Name)
	}
	taints := n.Spec.Taints
	if len(v.IgnoreTaints) > 0 {
		taints = nil
		for _, t := range n.Spec.Taints {
			if !v.IgnoreTaints[t.Key] {
				taints = append(taints, t)
			}
		}
	}
	if t, found := schedcorev1.FindMatchingUntoleratedTaint(klog.Background(), taints, pod.Spec.Tolerations, hardTaints, true); found {
		return fmt.Errorf("node %s has untolerated taint %s=%s:%s", n.Name, t.Key, t.Value, t.Effect)
	}
	if int64(v.Pods+1) > v.MaxPods {
		return fmt.Errorf("node %s is at its pod limit", n.Name)
	}
	for res, req := range PodRequests(pod) {
		if req.IsZero() {
			continue
		}
		free, ok := v.Free[res]
		if !ok {
			return fmt.Errorf("node %s has no %s", n.Name, res)
		}
		if free.Cmp(req) < 0 {
			return fmt.Errorf("node %s has insufficient %s (free %s, need %s)", n.Name, res, free.String(), req.String())
		}
	}
	for _, p := range hostPorts(pod) {
		if v.HostPorts[p] {
			return fmt.Errorf("node %s host port %s in use", n.Name, p)
		}
	}
	return nil
}

// Fits is Check()==nil.
func (v *NodeView) Fits(pod *corev1.Pod) bool { return v.Check(pod) == nil }
