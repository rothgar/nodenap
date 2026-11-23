package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PowerPolicySpec configures the scaler. Only one PowerPolicy is used (the
// first by name). Without one, the scaler only observes.
type PowerPolicySpec struct {
	// Report decisions without acting on any machine.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
	// Loop interval. Default 15s.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
	// Nodes counted as capacity. Default: all.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	// Nodes never powered off (control-plane nodes and the scaler's own node
	// always are).
	// +optional
	NeverPowerOffSelector *metav1.LabelSelector `json:"neverPowerOffSelector,omitempty"`
	// Pods that never wake a machine and never keep one on (per-machine
	// helpers, hardware managers). Annotation nodenap.rothgar.github.io/ignore=true
	// does the same per pod.
	// +optional
	IgnorePods []PodMatcher `json:"ignorePods,omitempty"`
	// Minimum Ready nodes to keep on.
	// +optional
	MinNodesOn int32 `json:"minNodesOn,omitempty"`
	// Spare schedulable capacity to keep on.
	// +optional
	Headroom Headroom `json:"headroom,omitempty"`
	// +optional
	ScaleUp ScaleUp `json:"scaleUp,omitempty"`
	// +optional
	ScaleDown ScaleDown `json:"scaleDown,omitempty"`
	// +optional
	Energy Energy `json:"energy,omitempty"`
	// +optional
	Integrations Integrations `json:"integrations,omitempty"`
	// Opt-in anonymous daily report of energy saved. Off by default.
	// +optional
	Telemetry Telemetry `json:"telemetry,omitempty"`
}

// Telemetry sends one anonymous report a day with cumulative counters only
// (no names, addresses, prices or money). See docs/savings-and-telemetry.md.
type Telemetry struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Collector URL the report is POSTed to.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// PodMatcher selects pods; all set fields must match.
type PodMatcher struct {
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// Headroom is free capacity summed over Ready nodes.
type Headroom struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// ScaleUp controls waking machines.
type ScaleUp struct {
	// How long a pod must be unschedulable first. Default 30s.
	// +optional
	PendingPodGracePeriod *metav1.Duration `json:"pendingPodGracePeriod,omitempty"`
	// Per powerOn driver. Default 10m.
	// +optional
	BootTimeout *metav1.Duration `json:"bootTimeout,omitempty"`
	// Machines booting at once. Default 3.
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
}

// ScaleDown controls powering machines off.
type ScaleDown struct {
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// A machine is a candidate when its pods' CPU and memory requests are
	// below this % of allocatable. Default 50.
	// +optional
	UtilizationThresholdPercent int32 `json:"utilizationThresholdPercent,omitempty"`
	// How long a machine must stay unneeded. Default 15m.
	// +optional
	UnneededTime *metav1.Duration `json:"unneededTime,omitempty"`
	// No power-offs this soon after a wake. Default 10m.
	// +optional
	DelayAfterScaleUp *metav1.Duration `json:"delayAfterScaleUp,omitempty"`
	// Machines powering off at once. Default 1.
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
	// Give up (and stay on) if pods haven't left after this. Default 10m.
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`
	// Time for the OS to shut down before powerCut / Failed. Default 5m.
	// +optional
	ShutdownTimeout *metav1.Duration `json:"shutdownTimeout,omitempty"`
	// Only power off if it saves at least this much per hour (decimal string,
	// currency units). Default 0.
	// +optional
	MinSavingsPerHour string `json:"minSavingsPerHour,omitempty"`
}

// Energy describes what electricity costs. All prices are per kWh.
type Energy struct {
	// IANA time zone for tariff windows. Default UTC.
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
	// Display only.
	// +optional
	Currency string `json:"currency,omitempty"`
	// +optional
	Tariff *Tariff `json:"tariff,omitempty"`
	// Live price per kWh (e.g. a Home Assistant sensor from your utility or
	// dynamic-tariff integration). Wins over the tariff when it has a value.
	// +optional
	Price *Value `json:"price,omitempty"`
	// What an exported kWh earns you. Ignored with netMetering.
	// +optional
	ExportPrice string `json:"exportPrice,omitempty"`
	// Retail net metering: exports are credited at the import price minus
	// these per-kWh charges.
	// +optional
	NetMetering *NetMetering `json:"netMetering,omitempty"`
	// Signed watts at the grid meter: positive = exporting, negative =
	// importing. Enables solar-aware pricing.
	// +optional
	GridPower *Value `json:"gridPower,omitempty"`
	// +optional
	Battery *Battery `json:"battery,omitempty"`
}

// Tariff is a time-of-use price table. The first matching window wins.
type Tariff struct {
	// Price outside all windows (decimal string).
	DefaultPrice string `json:"defaultPrice"`
	// +optional
	Windows []TariffWindow `json:"windows,omitempty"`
}

// TariffWindow is a price for part of the day. End before Start wraps
// midnight; Start == End is the whole day.
type TariffWindow struct {
	// Mon..Sun; empty is every day.
	// +optional
	Days []string `json:"days,omitempty"`
	// 1..12; empty is every month.
	// +optional
	Months []int32 `json:"months,omitempty"`
	// HH:MM local time.
	Start string `json:"start"`
	End   string `json:"end"`
	Price string `json:"price"`
}

// NetMetering prices exports at the import price minus charges billed only
// on consumption.
type NetMetering struct {
	// +optional
	NonBypassableChargesPerKWh string `json:"nonBypassableChargesPerKWh,omitempty"`
}

// Battery describes a home battery. It only counts as the source of extra
// load while it is above reserve and (when power is given) discharging.
type Battery struct {
	// State of charge, %.
	SoC Value `json:"soc"`
	// Watts, + discharging / − charging.
	// +optional
	Power *Value `json:"power,omitempty"`
	// +optional
	ReserveSoCPercent int32 `json:"reserveSoCPercent,omitempty"`
	// What a kWh from the battery is worth (usually the peak price it would
	// otherwise offset). Empty: the grid price.
	// +optional
	CostPerKWh string `json:"costPerKWh,omitempty"`
}

// Integrations are shared endpoints for Values and sensors.
type Integrations struct {
	// e.g. http://prometheus-operated.monitoring:9090
	// +optional
	PrometheusURL string `json:"prometheusURL,omitempty"`
	// e.g. http://homeassistant.home:8123
	// +optional
	HomeAssistantURL string `json:"homeAssistantURL,omitempty"`
	// Long-lived access token (key "token" by default).
	// +optional
	HomeAssistantTokenSecretRef *SecretKeyRef `json:"homeAssistantTokenSecretRef,omitempty"`
}

// PlannedAction is a decision from the last run.
type PlannedAction struct {
	Machine string `json:"machine"`
	Target  Target `json:"target"`
	Reason  string `json:"reason"`
}

// PowerPolicyStatus reports what the scaler sees and decided.
type PowerPolicyStatus struct {
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`
	// +optional
	Error string `json:"error,omitempty"`
	// lastActions in one line, e.g. "qb2=Off".
	// +optional
	Planned string `json:"planned,omitempty"`
	// suggestions in one line, e.g. "pi4-1=Off,pi4-2=Off".
	// +optional
	Suggested string `json:"suggested,omitempty"`
	// Import price now, per kWh.
	// +optional
	Price string `json:"price,omitempty"`
	// Marginal price of 100 extra watts now (solar/battery aware).
	// +optional
	EffectivePrice string `json:"effectivePrice,omitempty"`
	// +optional
	GridPowerWatts string `json:"gridPowerWatts,omitempty"`
	// +optional
	BatterySoCPercent string `json:"batterySoCPercent,omitempty"`
	// +optional
	MachinesWatts string `json:"machinesWatts,omitempty"`
	// +optional
	CostPerHour string `json:"costPerHour,omitempty"`
	// +optional
	NodesOn int32 `json:"nodesOn,omitempty"`
	// +optional
	NodesOff int32 `json:"nodesOff,omitempty"`
	// +optional
	PendingPods int32 `json:"pendingPods,omitempty"`
	// Decisions in the last run (in dry run: what would have happened).
	// +optional
	LastActions []PlannedAction `json:"lastActions,omitempty"`
	// What would happen to worker nodes that have no Machine yet, if they had
	// one: which could sleep (or should wake). Report-only.
	// +optional
	Suggestions []PlannedAction `json:"suggestions,omitempty"`
	// Pending pods no machine can run.
	// +optional
	UnsatisfiablePods []string `json:"unsatisfiablePods,omitempty"`
	// Problems reading energy data.
	// +optional
	Warnings []string `json:"warnings,omitempty"`
	// Running totals of what powering machines off has saved.
	// +optional
	Savings Savings `json:"savings,omitempty"`
}

// Savings are cumulative since Since. Only time a machine is fully Off
// counts; draining, shutting down and booting count as on.
type Savings struct {
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// +optional
	EnergyKWh string `json:"energyKWh,omitempty"`
	// In the policy's currency, at the price at the time.
	// +optional
	Cost string `json:"cost,omitempty"`
	// +optional
	MachineHoursOff string `json:"machineHoursOff,omitempty"`
	// +optional
	Wakes int64 `json:"wakes,omitempty"`
	// +optional
	FailedBoots int64 `json:"failedBoots,omitempty"`
	// Random, anonymous identifier for telemetry reports. Delete it to get a
	// new one.
	// +optional
	InstallID string `json:"installID,omitempty"`
	// +optional
	LastReportTime *metav1.Time `json:"lastReportTime,omitempty"`
	// +optional
	Reports int64 `json:"reports,omitempty"`
}

// PowerPolicy configures nodenap.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="DryRun",type=boolean,JSONPath=`.spec.dryRun`
// +kubebuilder:printcolumn:name="On",type=integer,JSONPath=`.status.nodesOn`
// +kubebuilder:printcolumn:name="Off",type=integer,JSONPath=`.status.nodesOff`
// +kubebuilder:printcolumn:name="Watts",type=string,JSONPath=`.status.machinesWatts`
// +kubebuilder:printcolumn:name="Price",type=string,JSONPath=`.status.effectivePrice`
// +kubebuilder:printcolumn:name="Cost/h",type=string,JSONPath=`.status.costPerHour`
// +kubebuilder:printcolumn:name="Planned",type=string,JSONPath=`.status.planned`
// +kubebuilder:printcolumn:name="Suggested",type=string,JSONPath=`.status.suggested`,priority=1
// +kubebuilder:printcolumn:name="Last Run",type=date,JSONPath=`.status.lastRunTime`
type PowerPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PowerPolicySpec   `json:"spec,omitempty"`
	Status            PowerPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PowerPolicyList is a list of PowerPolicy.
type PowerPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PowerPolicy `json:"items"`
}

func init() { SchemeBuilder.Register(&PowerPolicy{}, &PowerPolicyList{}) }
