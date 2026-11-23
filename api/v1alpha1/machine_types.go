package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PowerState is a machine's observed state.
// +kubebuilder:validation:Enum=Unknown;On;Draining;ShuttingDown;Off;Booting;Failed
type PowerState string

const (
	PowerUnknown      PowerState = "Unknown"
	PowerOn           PowerState = "On"
	PowerDraining     PowerState = "Draining"
	PowerShuttingDown PowerState = "ShuttingDown"
	PowerOff          PowerState = "Off"
	PowerBooting      PowerState = "Booting"
	PowerFailed       PowerState = "Failed"
)

// Target is what the planner (or an override) wants.
// +kubebuilder:validation:Enum=On;Off
type Target string

const (
	TargetOn  Target = "On"
	TargetOff Target = "Off"
)

// Profile is the machine's expected power use.
type Profile struct {
	// Watts when on and idle. Default 30.
	// +optional
	IdleWatts string `json:"idleWatts,omitempty"`
	// Watts at full load (caps the estimate).
	// +optional
	MaxWatts string `json:"maxWatts,omitempty"`
	// Extra watts per busy CPU core. Default 4.
	// +optional
	WattsPerCore string `json:"wattsPerCore,omitempty"`
	// Standby watts while off (0 when power is cut).
	// +optional
	OffWatts string `json:"offWatts,omitempty"`
	// Power-on to Node Ready. Default 3m.
	// +optional
	BootTime *metav1.Duration `json:"bootTime,omitempty"`
}

// MachineSpec says how to switch and measure one physical machine.
type MachineSpec struct {
	// Kubernetes node name. Defaults to the Machine's name.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// Never power off (still measured).
	// +optional
	AlwaysOn bool `json:"alwaysOn,omitempty"`
	// Higher is preferred to stay on and to be woken (tie-breaker).
	// +optional
	Priority int32 `json:"priority,omitempty"`
	// Ways to power on, tried in order; each gets bootTimeout.
	// +optional
	PowerOn []Driver `json:"powerOn,omitempty"`
	// Graceful OS shutdown.
	// +optional
	Shutdown *Driver `json:"shutdown,omitempty"`
	// Cut power after the node is NotReady (or the shutdown timed out),
	// e.g. a smart plug for 0 W standby. Don't use with Wake-on-LAN.
	// +optional
	PowerCut *Driver `json:"powerCut,omitempty"`
	// Wait after NotReady before cutting power. Default 30s.
	// +optional
	PowerCutDelay *metav1.Duration `json:"powerCutDelay,omitempty"`
	// Where the machine's watts come from.
	// +optional
	Sensor *Driver `json:"sensor,omitempty"`
	// Overrides the policy's scaleUp.bootTimeout.
	// +optional
	BootTimeout *metav1.Duration `json:"bootTimeout,omitempty"`
	// +optional
	Profile Profile `json:"profile,omitempty"`
}

// CachedNode is the Node as last seen while on: what the planner schedules
// against while the machine is off.
type CachedNode struct {
	Labels      map[string]string   `json:"labels,omitempty"`
	Taints      []corev1.Taint      `json:"taints,omitempty"`
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`
	// Requests of the DaemonSet pods that will start when it boots.
	DaemonSetRequests corev1.ResourceList `json:"daemonSetRequests,omitempty"`
	CapturedAt        metav1.Time         `json:"capturedAt"`
}

// MachineStatus is the observed state.
type MachineStatus struct {
	// +optional
	PowerState PowerState `json:"powerState,omitempty"`
	// +optional
	Target Target `json:"target,omitempty"`
	// +optional
	TargetReason string `json:"targetReason,omitempty"`
	// Dry run only: the target nodenap would set now (empty: no change).
	// +optional
	WouldTarget Target `json:"wouldTarget,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// When the planner first saw the machine as unneeded.
	// +optional
	UnneededSince *metav1.Time `json:"unneededSince,omitempty"`
	// Set when the scaler itself powered the machine off. Only then are pods
	// still bound to the node force-deleted.
	// +optional
	PoweredOffAt *metav1.Time `json:"poweredOffAt,omitempty"`
	// Index into spec.powerOn being tried.
	// +optional
	PowerOnAttempt int32 `json:"powerOnAttempt,omitempty"`
	// +optional
	FailureCount int32 `json:"failureCount,omitempty"`
	// +optional
	CachedNode *CachedNode `json:"cachedNode,omitempty"`
	// +optional
	MeasuredWatts string `json:"measuredWatts,omitempty"`
	// +optional
	MeasuredAt *metav1.Time `json:"measuredAt,omitempty"`
}

// Machine is a physical machine whose power nodenap controls.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.powerState`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.target`
// +kubebuilder:printcolumn:name="Would",type=string,JSONPath=`.status.wouldTarget`,description="dry run: target nodenap would set"
// +kubebuilder:printcolumn:name="Watts",type=string,JSONPath=`.status.measuredWatts`
// +kubebuilder:printcolumn:name="Idle",type=date,JSONPath=`.status.unneededSince`,description="time since the machine became unneeded"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.targetReason`,priority=1
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MachineSpec   `json:"spec,omitempty"`
	Status            MachineStatus `json:"status,omitempty"`
}

// NodeName returns spec.nodeName or the object name.
func (m *Machine) NodeName() string {
	if m.Spec.NodeName != "" {
		return m.Spec.NodeName
	}
	return m.Name
}

// +kubebuilder:object:root=true

// MachineList is a list of Machines.
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}

func init() { SchemeBuilder.Register(&Machine{}, &MachineList{}) }
