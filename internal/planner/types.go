// Package planner decides which machines to power on or off. It is a pure
// function of its Input so it can be tested without a cluster.
package planner

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/energy"
)

// Default power estimates for machines with no profile and no samples.
const (
	DefaultIdleWatts    = 30.0 // when a machine's size is unknown
	DefaultWattsPerCore = 4.0
	DefaultBootTime     = 3 * time.Minute
)

// EstimateIdleWatts guesses idle power from a machine's size when nothing
// better is known: 5 W plus 2 W per CPU core (a 4-core Raspberry Pi ≈ 13 W,
// a 12-core mini PC ≈ 29 W, a 20-core workstation ≈ 45 W).
func EstimateIdleWatts(allocatable corev1.ResourceList) float64 {
	q, ok := allocatable[corev1.ResourceCPU]
	if !ok {
		return DefaultIdleWatts
	}
	return 5 + 2*float64(q.MilliValue())/1000
}

// Profile is a machine's (configured or learned) power behaviour.
type Profile struct {
	IdleWatts    float64
	MaxWatts     float64 // 0 = unknown
	WattsPerCore float64
	OffWatts     float64
	BootTime     time.Duration
}

// Estimate returns expected watts with cores of CPU busy.
func (p Profile) Estimate(cores float64) float64 {
	w := p.IdleWatts + p.WattsPerCore*cores
	if p.MaxWatts > 0 && w > p.MaxWatts {
		w = p.MaxWatts
	}
	return w
}

// Node is everything the planner knows about one node.
type Node struct {
	Name string
	// Managed nodes have a ManagedNode and can be powered on/off.
	Managed bool
	// Protected nodes are never powered off (control plane, the controller's
	// own node, neverPowerOffSelector, alwaysOn).
	Protected       bool
	ProtectedReason string
	CanPowerOn      bool
	CanPowerOff     bool
	State           v1alpha1.PowerState
	Target          v1alpha1.Target
	// Ready is the live Node Ready condition.
	Ready bool
	// Live Node object; nil if the Node doesn't exist (yet).
	Node   *corev1.Node
	Cached *v1alpha1.CachedNode
	// Pods bound to this node that are not terminal.
	Pods          []*corev1.Pod
	Profile       Profile
	MeasuredWatts *float64
	Priority      int32
	UnneededSince *time.Time
	// Override from the nodenap.rothgar.github.io/override annotation.
	Override v1alpha1.Target
	// UpgradeFrom/UpgradeAt: the versions the node reported when it was last
	// woken to catch up with an upgrade, and when.
	UpgradeFrom string
	UpgradeAt   time.Time
}

// Settings are the PowerPolicy knobs with defaults applied.
type Settings struct {
	MinNodesOn           int
	UtilizationThreshold float64 // 0..1
	UnneededTime         time.Duration
	DelayAfterScaleUp    time.Duration
	MaxConcurrentUp      int
	MaxConcurrentDown    int
	MinSavingsPerHour    float64
	ScaleDownDisabled    bool
	HeadroomCPU          *resource.Quantity
	HeadroomMemory       *resource.Quantity
}

// Input is a snapshot of the world.
type Input struct {
	Now   time.Time
	Nodes []*Node
	// PendingPods are unschedulable pods older than the grace period.
	PendingPods []*corev1.Pod
	Energy      energy.Snapshot
	Settings    Settings
	LastScaleUp time.Time
	// Ignored reports pods that never wake machines and never block
	// powering a node off. Nil ignores nothing.
	Ignored func(*corev1.Pod) bool
	// Upgrade describes a cluster upgrade in progress, reported by the
	// management plane (e.g. Omni). While set, off machines are woken and
	// nothing is powered off.
	Upgrade string
}

func (in Input) ignored(p *corev1.Pod) bool { return in.Ignored != nil && in.Ignored(p) }

// Action changes a node's target state.
type Action struct {
	Node   string
	Target v1alpha1.Target
	Reason string
	// UpgradeFrom is set when the machine is woken to catch up with an
	// upgrade: the versions its node reports now.
	UpgradeFrom string
}

// Result is the plan.
type Result struct {
	Actions []Action
	// Unneeded maps nodes that are currently unneeded to since when.
	Unneeded      map[string]time.Time
	Unsatisfiable []string
	// Estimated draw and cost of the managed fleet right now.
	EstimatedWatts       float64
	EstimatedCostPerHour float64
	NodesOn, NodesOff    int
	Notes                []string
}
