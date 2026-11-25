// Package scaler is nodenap's single control loop: every interval it
// reads the cluster and energy data, plans which machines should be on, and
// steps each machine's power state machine toward that plan.
package scaler

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/drivers"
	"github.com/rothgar/nodenap/internal/energy"
	"github.com/rothgar/nodenap/internal/metrics"
	"github.com/rothgar/nodenap/internal/planner"
	"github.com/rothgar/nodenap/internal/sources"
)

// +kubebuilder:rbac:groups=power.nodenap.rothgar.github.io,resources=machines;powerpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=power.nodenap.rothgar.github.io,resources=machines/status;powerpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,namespace=nodenap
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Defaults for PowerPolicy fields.
const (
	defaultInterval     = 15 * time.Second
	defaultPendingGrace = 30 * time.Second
	defaultBootTimeout  = 10 * time.Minute
	defaultDrainTimeout = 10 * time.Minute
	defaultShutdownTime = 5 * time.Minute
	defaultUnneededTime = 15 * time.Minute
	defaultDelayAfterUp = 10 * time.Minute
	defaultUtilization  = 50
	defaultMaxUp        = 3
	defaultMaxDown      = 1
)

// Scaler is the control loop.
type Scaler struct {
	Client client.Client
	// SecretReader reads Secrets uncached (no cluster-wide Secret watch).
	SecretReader client.Reader
	Recorder     recorder.EventRecorder
	Namespace    string // the scaler's namespace (default for secret refs)
	// The controller's own pod ("namespace/name"). When its node is drained
	// it is evicted last; the replacement on another node resumes from the
	// Machine status and finishes the power-off.
	SelfPod string
	// Optional hooks (tests, powerplan).
	NewPrometheus func(url string) *sources.Prometheus
	NewOmni       func(endpoint, key string) (drivers.OmniAPI, error)
	Now           func() time.Time

	Drivers     *drivers.Env // rebuilt every run from the policy's integrations
	mu          sync.Mutex
	lastScaleUp time.Time
	dryRunSeen  map[string]v1alpha1.Target

	// cluster upgrade reported by the management plane, checked every upgradeCheck
	upgrade        string
	upgradeChecked time.Time

	// savings accounting
	lastRun           time.Time
	run               tally
	wakeSeconds       []float64
	lastReportAttempt time.Time
	suggestUnneeded   map[string]time.Time
}

// NeedLeaderElection: only the leader runs the loop.
func (s *Scaler) NeedLeaderElection() bool { return true }

func (s *Scaler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scaler) event(obj client.Object, typ, reason, msg string, args ...any) {
	if s.Recorder != nil {
		s.Recorder.Eventf(obj, nil, typ, reason, reason, msg, args...)
	}
}

// Start runs the loop until ctx is cancelled.
func (s *Scaler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("nodenap")
	for {
		next, err := s.RunOnce(log.IntoContext(ctx, logger))
		if err != nil {
			metrics.Errors.Inc()
			logger.Error(err, "loop failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(next):
		}
	}
}

func dur(d *metav1.Duration, def time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return def
	}
	return d.Duration
}

func num(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}

// RunOnce does one gather → plan → act pass and returns when to run next.
func (s *Scaler) RunOnce(ctx context.Context) (time.Duration, error) {
	now := s.now()
	var policies v1alpha1.PowerPolicyList
	if err := s.Client.List(ctx, &policies); err != nil {
		return defaultInterval, err
	}
	var policy *v1alpha1.PowerPolicy
	for i := range policies.Items {
		if policy == nil || policies.Items[i].Name < policy.Name {
			policy = &policies.Items[i]
		}
	}
	var spec v1alpha1.PowerPolicySpec
	if policy != nil {
		spec = policy.Spec
	}
	interval := dur(spec.Interval, defaultInterval)

	var nodeList corev1.NodeList
	var machineList v1alpha1.MachineList
	var podList corev1.PodList
	for _, l := range []client.ObjectList{&nodeList, &machineList, &podList} {
		if err := s.Client.List(ctx, l); err != nil {
			return interval, err
		}
	}
	nodes := map[string]*corev1.Node{}
	for i := range nodeList.Items {
		nodes[nodeList.Items[i].Name] = &nodeList.Items[i]
	}
	podsByNode := map[string][]*corev1.Pod{}
	var pending []*corev1.Pod
	grace := dur(spec.ScaleUp.PendingPodGracePeriod, defaultPendingGrace)
	for i := range podList.Items {
		p := &podList.Items[i]
		switch {
		case p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed:
		case p.Spec.NodeName != "":
			podsByNode[p.Spec.NodeName] = append(podsByNode[p.Spec.NodeName], p)
		case p.DeletionTimestamp == nil:
			if since, ok := unschedulableSince(p); ok && now.Sub(since) >= grace {
				pending = append(pending, p)
			}
		}
	}

	if err := s.buildDrivers(ctx, spec.Integrations); err != nil {
		return interval, s.fail(ctx, policy, err)
	}

	machines := machineList.Items
	sort.Slice(machines, func(i, j int) bool { return machines[i].Name < machines[j].Name })

	if policy == nil {
		for i := range machines {
			m := &machines[i]
			x := &machineStep{s: s, m: m, node: nodes[m.NodeName()], pods: podsByNode[m.NodeName()]}
			orig := m.DeepCopy()
			x.observe("no PowerPolicy")
			s.patchStatus(ctx, orig, m)
		}
		return interval, nil
	}

	settings, ignored, err := plannerSettings(spec)
	if err != nil {
		return interval, s.fail(ctx, policy, err)
	}
	in, err := s.plannerInput(ctx, spec, nodes, machines, podsByNode, pending, now)
	if err != nil {
		return interval, s.fail(ctx, policy, err)
	}
	in.Settings, in.Ignored = settings, ignored
	in.Energy = energy.Read(ctx, spec.Energy, s.Drivers.Resolver, now)
	in.Upgrade = s.clusterUpgrade(ctx, machines, now)
	s.mu.Lock()
	in.LastScaleUp = s.lastScaleUp
	s.mu.Unlock()

	res := planner.Plan(in)
	var dt time.Duration
	if !s.lastRun.IsZero() {
		dt = min(now.Sub(s.lastRun), 2*interval)
	}
	s.lastRun = now
	s.act(ctx, spec, machines, nodes, podsByNode, in, res, dt)
	suggestions := s.suggest(ctx, in)
	s.publish(ctx, policy, in, res, len(machines), suggestions)
	return interval, nil
}

func (s *Scaler) buildDrivers(ctx context.Context, in v1alpha1.Integrations) error {
	r := &sources.Resolver{Reader: s.SecretReader, Namespace: s.Namespace}
	if r.Reader == nil {
		r.Reader = s.Client
	}
	newProm := sources.NewPrometheus
	if s.NewPrometheus != nil {
		newProm = s.NewPrometheus
	}
	r.Prom = newProm(in.PrometheusURL)
	if in.HomeAssistantURL != "" {
		token := ""
		if ref := in.HomeAssistantTokenSecretRef; ref != nil {
			t, err := r.SecretKey(ctx, *ref, "token")
			if err != nil {
				return fmt.Errorf("home assistant token: %w", err)
			}
			token = t
		}
		r.HA = sources.NewHomeAssistant(in.HomeAssistantURL, token)
	}
	s.Drivers = &drivers.Env{Resolver: r, NewOmni: s.NewOmni}
	return nil
}

func (s *Scaler) fail(ctx context.Context, p *v1alpha1.PowerPolicy, err error) error {
	if p != nil {
		orig := p.DeepCopy()
		p.Status.Error = err.Error()
		_ = s.Client.Status().Patch(ctx, p, client.MergeFrom(orig))
	}
	return err
}

func plannerSettings(spec v1alpha1.PowerPolicySpec) (planner.Settings, func(*corev1.Pod) bool, error) {
	sd := spec.ScaleDown
	minSave, err := energy.ParseDecimal(sd.MinSavingsPerHour, 0)
	if err != nil {
		return planner.Settings{}, nil, fmt.Errorf("scaleDown.minSavingsPerHour: %w", err)
	}
	ignored, err := ignoreMatcher(spec.IgnorePods)
	if err != nil {
		return planner.Settings{}, nil, err
	}
	return planner.Settings{
		MinNodesOn:           int(spec.MinNodesOn),
		UtilizationThreshold: float64(num(sd.UtilizationThresholdPercent, defaultUtilization)) / 100,
		UnneededTime:         dur(sd.UnneededTime, defaultUnneededTime),
		DelayAfterScaleUp:    dur(sd.DelayAfterScaleUp, defaultDelayAfterUp),
		MaxConcurrentUp:      int(num(spec.ScaleUp.MaxConcurrent, defaultMaxUp)),
		MaxConcurrentDown:    int(num(sd.MaxConcurrent, defaultMaxDown)),
		MinSavingsPerHour:    minSave,
		ScaleDownDisabled:    sd.Disabled,
		HeadroomCPU:          spec.Headroom.CPU,
		HeadroomMemory:       spec.Headroom.Memory,
	}, ignored, nil
}

// profile merges the configured profile with defaults.
func profile(m *v1alpha1.Machine) planner.Profile {
	p := m.Spec.Profile
	f := func(v string, def float64) float64 {
		x, err := energy.ParseDecimal(v, def)
		if err != nil {
			return def
		}
		return x
	}
	return planner.Profile{
		IdleWatts:    f(p.IdleWatts, defaultIdle(m)),
		WattsPerCore: f(p.WattsPerCore, planner.DefaultWattsPerCore),
		MaxWatts:     f(p.MaxWatts, 0),
		OffWatts:     f(p.OffWatts, 0),
		BootTime:     dur(p.BootTime, planner.DefaultBootTime),
	}
}

// defaultIdle estimates idle watts from the machine's cached size.
func defaultIdle(m *v1alpha1.Machine) float64 {
	if m.Status.CachedNode == nil {
		return planner.DefaultIdleWatts
	}
	return planner.EstimateIdleWatts(m.Status.CachedNode.Allocatable)
}

// plannerInput builds the planner's view, measuring machines in parallel.
func (s *Scaler) plannerInput(ctx context.Context, spec v1alpha1.PowerPolicySpec, nodes map[string]*corev1.Node,
	machines []v1alpha1.Machine, podsByNode map[string][]*corev1.Pod, pending []*corev1.Pod, now time.Time) (planner.Input, error) {
	in := planner.Input{Now: now, PendingPods: pending}
	sel, never := labels.Everything(), labels.Nothing()
	var err error
	if spec.NodeSelector != nil {
		if sel, err = metav1.LabelSelectorAsSelector(spec.NodeSelector); err != nil {
			return in, fmt.Errorf("nodeSelector: %w", err)
		}
	}
	if spec.NeverPowerOffSelector != nil {
		if never, err = metav1.LabelSelectorAsSelector(spec.NeverPowerOffSelector); err != nil {
			return in, fmt.Errorf("neverPowerOffSelector: %w", err)
		}
	}
	byNode := map[string]*v1alpha1.Machine{}
	for i := range machines {
		byNode[machines[i].NodeName()] = &machines[i]
	}

	var wg sync.WaitGroup
	add := func(name string, node *corev1.Node) {
		pn := &planner.Node{Name: name, Node: node, Ready: NodeReady(node), Pods: podsByNode[name], State: v1alpha1.PowerOn}
		if node != nil {
			switch {
			case isControlPlane(node):
				pn.Protected, pn.ProtectedReason = true, "control-plane node"
			case never.Matches(labels.Set(node.Labels)):
				pn.Protected, pn.ProtectedReason = true, "matches neverPowerOffSelector"
			}
		}
		if m := byNode[name]; m != nil {
			pn.Managed = true
			pn.State = m.Status.PowerState
			if pn.State == "" || pn.State == v1alpha1.PowerUnknown {
				// Not adopted yet: a Ready node counts as on, anything else as unknown.
				pn.State = v1alpha1.PowerUnknown
				if pn.Ready {
					pn.State = v1alpha1.PowerOn
				}
			}
			pn.Target, pn.Cached, pn.Profile, pn.Priority = m.Status.Target, m.Status.CachedNode, profile(m), m.Spec.Priority
			pn.CanPowerOn = len(m.Spec.PowerOn) > 0
			pn.CanPowerOff = m.Spec.Shutdown != nil || m.Spec.PowerCut != nil
			if m.Spec.AlwaysOn && !pn.Protected {
				pn.Protected, pn.ProtectedReason = true, "alwaysOn"
			}
			if m.Status.UnneededSince != nil {
				t := m.Status.UnneededSince.Time
				pn.UnneededSince = &t
			}
			if w := m.Status.UpgradeWake; w != nil {
				pn.UpgradeFrom, pn.UpgradeAt = w.From, w.At.Time
			}
			switch strings.ToLower(m.Annotations[v1alpha1.OverrideAnnotation]) {
			case "on":
				pn.Override = v1alpha1.TargetOn
			case "off":
				pn.Override = v1alpha1.TargetOff
			}
			if m.Spec.Sensor != nil && pn.State == v1alpha1.PowerOn && pn.Ready {
				wg.Add(1)
				go func() {
					defer wg.Done()
					mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					if w, err := s.Drivers.Watts(mctx, *m.Spec.Sensor, drivers.Machine{NodeName: name, Node: node}); err == nil {
						pn.MeasuredWatts = &w
					} else {
						log.FromContext(ctx).V(1).Info("sensor failed", "machine", m.Name, "err", err.Error())
					}
				}()
			}
		}
		in.Nodes = append(in.Nodes, pn)
	}
	for name, node := range nodes {
		if sel.Matches(labels.Set(node.Labels)) {
			add(name, node)
		}
	}
	for name, m := range byNode { // Machines whose Node object is gone
		if nodes[name] == nil && (m.Status.CachedNode == nil || sel.Matches(labels.Set(m.Status.CachedNode.Labels))) {
			add(name, nil)
		}
	}
	wg.Wait()
	sort.Slice(in.Nodes, func(i, j int) bool { return in.Nodes[i].Name < in.Nodes[j].Name })
	return in, nil
}

// act writes plan results to Machines and steps each machine.
func (s *Scaler) act(ctx context.Context, spec v1alpha1.PowerPolicySpec, machines []v1alpha1.Machine,
	nodes map[string]*corev1.Node, podsByNode map[string][]*corev1.Pod, in planner.Input, res planner.Result, dt time.Duration) {
	actions := map[string]planner.Action{}
	for _, a := range res.Actions {
		actions[a.Node] = a
		metrics.Actions.WithLabelValues(string(a.Target), strconv.FormatBool(spec.DryRun)).Inc()
		log.FromContext(ctx).Info("plan", "machine", a.Node, "target", a.Target, "reason", a.Reason, "dryRun", spec.DryRun)
	}
	measured := map[string]*float64{}
	for _, n := range in.Nodes {
		measured[n.Name] = n.MeasuredWatts
	}
	to := timeouts{
		boot:     dur(spec.ScaleUp.BootTimeout, defaultBootTimeout),
		drain:    dur(spec.ScaleDown.DrainTimeout, defaultDrainTimeout),
		shutdown: dur(spec.ScaleDown.ShutdownTimeout, defaultShutdownTime),
	}

	for i := range machines {
		m := &machines[i]
		name := m.NodeName()
		orig := m.DeepCopy()
		st := &m.Status
		if st.Target == "" {
			st.Target, st.TargetReason = v1alpha1.TargetOff, "initial state"
			if NodeReady(nodes[name]) {
				st.Target = v1alpha1.TargetOn
			}
		}
		a, planned := actions[name]
		switch {
		case planned && !spec.DryRun:
			st.Target, st.TargetReason = a.Target, a.Reason
			if a.UpgradeFrom != "" {
				st.UpgradeWake = &v1alpha1.UpgradeWake{From: a.UpgradeFrom, At: metav1.NewTime(in.Now)}
			}
			s.event(m, corev1.EventTypeNormal, "Target"+string(a.Target), "%s", a.Reason)
			if a.Target == v1alpha1.TargetOn {
				s.mu.Lock()
				s.lastScaleUp = in.Now
				s.mu.Unlock()
			}
		case spec.DryRun:
			s.auditDryRun(m, a, planned)
		}
		st.WouldTarget = ""
		if spec.DryRun && planned {
			st.WouldTarget = a.Target
		}
		if since, ok := res.Unneeded[name]; ok {
			if st.UnneededSince == nil || !st.UnneededSince.Time.Equal(since) {
				st.UnneededSince = &metav1.Time{Time: since}
			}
		} else if !(spec.DryRun && planned && a.Target == v1alpha1.TargetOff) {
			// A dry-run decision never takes effect, so the machine stays
			// unneeded: keep its timer, or the next loop restarts it and the
			// scale-down slot goes to another machine.
			st.UnneededSince = nil
		}
		if w := measured[name]; w != nil {
			old, _ := energy.ParseDecimal(st.MeasuredWatts, -1000)
			if st.MeasuredAt == nil || in.Now.Sub(st.MeasuredAt.Time) > 5*time.Minute || abs(old-*w) >= 1 {
				st.MeasuredWatts = strconv.FormatFloat(*w, 'f', 1, 64)
				st.MeasuredAt = &metav1.Time{Time: in.Now}
			}
		}

		x := &machineStep{s: s, m: m, node: nodes[name], pods: podsByNode[name], to: to}
		x.to.boot = dur(m.Spec.BootTimeout, to.boot)
		if spec.DryRun {
			x.observe("dry run")
		} else if err := x.step(ctx); err != nil {
			st.Message = "error: " + err.Error()
			s.event(m, corev1.EventTypeWarning, "Error", "%v", err)
			log.FromContext(ctx).Error(err, "step failed", "machine", m.Name, "state", st.PowerState)
		}
		s.accountOff(m, in.Energy, dt)
		s.patchStatus(ctx, orig, m)
	}
}

// auditDryRun emits WouldPowerOn/WouldPowerOff when a dry-run decision changes.
func (s *Scaler) auditDryRun(m *v1alpha1.Machine, a planner.Action, planned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dryRunSeen == nil {
		s.dryRunSeen = map[string]v1alpha1.Target{}
	}
	var t v1alpha1.Target
	if planned {
		t = a.Target
	}
	if s.dryRunSeen[m.Name] == t {
		return
	}
	s.dryRunSeen[m.Name] = t
	if planned {
		s.event(m, corev1.EventTypeNormal, "WouldPower"+string(t), "dry run: %s", a.Reason)
	}
}

func (s *Scaler) patchStatus(ctx context.Context, orig, m *v1alpha1.Machine) {
	if equality.Semantic.DeepEqual(orig.Status, m.Status) {
		return
	}
	if err := s.Client.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
		log.FromContext(ctx).Error(err, "updating Machine status", "machine", m.Name)
	}
}

// oneLine renders actions as "a=Off,b=On" for kubectl columns.
func oneLine(actions []v1alpha1.PlannedAction) string {
	parts := make([]string, len(actions))
	for i, a := range actions {
		parts[i] = a.Machine + "=" + string(a.Target)
	}
	return strings.Join(parts, ",")
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func fmtF(v float64, prec int) string { return strconv.FormatFloat(v, 'f', prec, 64) }

// publish updates the PowerPolicy status and metrics.
func (s *Scaler) publish(ctx context.Context, p *v1alpha1.PowerPolicy, in planner.Input, res planner.Result, machines int, suggestions []v1alpha1.PlannedAction) {
	e := in.Energy
	orig := p.DeepCopy()
	st := &p.Status
	pending := int32(0)
	for _, pod := range in.PendingPods {
		if in.Ignored == nil || !in.Ignored(pod) {
			pending++
		}
	}
	savings := orig.Status.Savings
	s.addSavings(&savings, in.Now)
	publishLifetime(savings)
	s.maybeReport(ctx, p.Spec.Telemetry, &savings, machines, in.Now)
	*st = v1alpha1.PowerPolicyStatus{
		Savings:           savings,
		LastRunTime:       &metav1.Time{Time: in.Now},
		Price:             fmtF(e.GridPrice, 4),
		EffectivePrice:    fmtF(e.MarginalPrice(100), 4),
		MachinesWatts:     fmtF(res.EstimatedWatts, 1),
		CostPerHour:       fmtF(res.EstimatedCostPerHour, 4),
		NodesOn:           int32(res.NodesOn),
		NodesOff:          int32(res.NodesOff),
		PendingPods:       pending,
		UnsatisfiablePods: res.Unsatisfiable,
		Suggestions:       suggestions,
		Warnings:          e.Warnings,
	}
	if e.GridPowerWatts != nil {
		st.GridPowerWatts = fmtF(*e.GridPowerWatts, 1)
		metrics.GridPower.Set(*e.GridPowerWatts)
	}
	if e.BatterySoC != nil {
		st.BatterySoCPercent = fmtF(*e.BatterySoC, 1)
		metrics.BatterySoC.Set(*e.BatterySoC)
	}
	for _, a := range res.Actions {
		st.LastActions = append(st.LastActions, v1alpha1.PlannedAction{Machine: a.Node, Target: a.Target, Reason: a.Reason})
	}
	st.Planned, st.Suggested = oneLine(st.LastActions), oneLine(st.Suggestions)
	if !equality.Semantic.DeepEqual(orig.Status, p.Status) {
		if err := s.Client.Status().Patch(ctx, p, client.MergeFrom(orig)); err != nil {
			log.FromContext(ctx).Error(err, "updating PowerPolicy status")
		}
	}

	metrics.Price.Set(e.GridPrice)
	metrics.EffectivePrice.Set(e.MarginalPrice(100))
	metrics.MachinesWatts.Set(res.EstimatedWatts)
	metrics.CostPerHour.Set(res.EstimatedCostPerHour)
	metrics.PendingPods.Set(float64(pending))
	metrics.MachineState.Reset()
	metrics.MachineWatts.Reset()
	for _, n := range in.Nodes {
		if !n.Managed {
			continue
		}
		metrics.MachineState.WithLabelValues(n.Name, string(n.State)).Set(1)
		if n.MeasuredWatts != nil {
			metrics.MachineWatts.WithLabelValues(n.Name, "measured").Set(*n.MeasuredWatts)
		}
	}
}

const upgradeCheck = time.Minute

// clusterUpgrade asks the management plane (Omni) through the first machine
// with an Omni driver whether the cluster is being upgraded. On error the
// last answer stands, so a flaky API doesn't end an upgrade pause early.
func (s *Scaler) clusterUpgrade(ctx context.Context, machines []v1alpha1.Machine, now time.Time) string {
	if now.Sub(s.upgradeChecked) < upgradeCheck {
		return s.upgrade
	}
	s.upgradeChecked = now
	for i := range machines {
		m := &machines[i]
		d := omniDriver(m.Spec)
		if d == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		u, err := s.Drivers.Upgrade(cctx, *d, m.NodeName())
		cancel()
		if err != nil {
			log.FromContext(ctx).Info("checking for a cluster upgrade failed", "machine", m.Name, "err", err.Error())
			return s.upgrade
		}
		if u != s.upgrade {
			log.FromContext(ctx).Info("cluster upgrade", "status", u)
		}
		s.upgrade = u
		return u
	}
	s.upgrade = ""
	return ""
}

func omniDriver(spec v1alpha1.MachineSpec) *v1alpha1.Driver {
	for _, d := range append([]*v1alpha1.Driver{spec.Shutdown, spec.PowerCut}, ptrs(spec.PowerOn)...) {
		if d != nil && d.Omni != nil {
			return d
		}
	}
	return nil
}

func ptrs(ds []v1alpha1.Driver) []*v1alpha1.Driver {
	out := make([]*v1alpha1.Driver, len(ds))
	for i := range ds {
		out[i] = &ds[i]
	}
	return out
}
