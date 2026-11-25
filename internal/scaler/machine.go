package scaler

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/drivers"
	"github.com/rothgar/nodenap/internal/planner"
)

// Each machine moves toward status.target, one step per loop:
//
//	On ─(target Off)→ Draining ─(pods gone)→ ShuttingDown ─(NotReady)→ Off
//	Off ─(target On)→ Booting ─(Ready)→ On
//	Booting ─(every powerOn driver timed out)→ Failed ─(backoff)→ Off → Booting
const (
	maxFailureBackoff       = time.Hour
	unexpectedNotReadyGrace = 5 * time.Minute
	// offSettle: kubelet status posted this soon after Off isn't taken as a manual power-on.
	offSettle            = 3 * time.Minute
	strandedPodGrace     = time.Minute
	defaultPowerCutDelay = 30 * time.Second
)

type timeouts struct{ boot, drain, shutdown time.Duration }

// machineStep is one loop's view of a machine.
type machineStep struct {
	s    *Scaler
	m    *v1alpha1.Machine
	node *corev1.Node // nil if the Node doesn't exist
	pods []*corev1.Pod
	to   timeouts
}

func (x *machineStep) target() drivers.Machine {
	return drivers.Machine{NodeName: x.m.NodeName(), Node: x.node}
}

func (x *machineStep) transition(to v1alpha1.PowerState, msg string) {
	st := &x.m.Status
	if to != v1alpha1.PowerOff {
		st.PoweredOffAt = nil
	}
	if st.PowerState != to {
		x.s.event(x.m, corev1.EventTypeNormal, string(to), "%s → %s: %s", st.PowerState, to, msg)
	}
	st.PowerState, st.Message = to, msg
	st.LastTransitionTime = &metav1.Time{Time: x.s.now()}
}

func (x *machineStep) since() time.Duration {
	if x.m.Status.LastTransitionTime == nil {
		return 0
	}
	return x.s.now().Sub(x.m.Status.LastTransitionTime.Time)
}

// refreshSnapshot keeps the capability cache fresh while the machine is up.
func (x *machineStep) refreshSnapshot() {
	switch x.m.Status.PowerState {
	case "", v1alpha1.PowerUnknown, v1alpha1.PowerOn, v1alpha1.PowerBooting:
	default:
		return
	}
	if !NodeReady(x.node) {
		return
	}
	snap := Snapshot(x.node, x.pods, x.s.now())
	if c := x.m.Status.CachedNode; c == nil || snapshotChanged(c, snap) || x.s.now().Sub(c.CapturedAt.Time) > time.Hour {
		x.m.Status.CachedNode = snap
	}
}

// observe tracks state without side effects (dry run, or no PowerPolicy). A
// machine caught mid-transition is left exactly as it is.
func (x *machineStep) observe(why string) {
	x.refreshSnapshot()
	switch x.m.Status.PowerState {
	case "", v1alpha1.PowerUnknown, v1alpha1.PowerOn, v1alpha1.PowerOff:
		want, msg := v1alpha1.PowerOff, "node is not Ready"
		if NodeReady(x.node) {
			want, msg = v1alpha1.PowerOn, "node is Ready"
		}
		if x.m.Status.PowerState != want {
			x.transition(want, msg+" (observing: "+why+")")
		}
	default:
		x.m.Status.Message = "paused in " + string(x.m.Status.PowerState) + ": " + why
	}
}

func (x *machineStep) untaint(ctx context.Context) error {
	if x.node == nil {
		return nil
	}
	return setTaint(ctx, x.s.Client, x.node, "")
}

// step advances the state machine once.
func (x *machineStep) step(ctx context.Context) error {
	x.refreshSnapshot()
	st := &x.m.Status
	ready := NodeReady(x.node)

	switch st.PowerState {
	case "", v1alpha1.PowerUnknown:
		switch {
		case ready && powerTaint(x.node) != nil && st.Target == v1alpha1.TargetOff:
			x.transition(v1alpha1.PowerDraining, "resuming drain after restart")
		case ready:
			x.transition(v1alpha1.PowerOn, "node is Ready")
		default:
			x.transition(v1alpha1.PowerOff, "node is not Ready")
		}
		return nil

	case v1alpha1.PowerOn:
		if x.node != nil && !ready && x.s.now().Sub(NotReadySince(x.node)) > unexpectedNotReadyGrace {
			x.transition(v1alpha1.PowerOff, "node went NotReady unexpectedly")
			return nil
		}
		if st.Target == v1alpha1.TargetOff && x.node != nil {
			if err := setTaint(ctx, x.s.Client, x.node, corev1.TaintEffectNoSchedule); err != nil {
				return err
			}
			x.transition(v1alpha1.PowerDraining, "cordoned, evicting pods")
			return nil
		}
		return x.untaint(ctx) // left over from an aborted power-off

	case v1alpha1.PowerDraining:
		return x.drain(ctx)

	case v1alpha1.PowerShuttingDown:
		return x.shuttingDown(ctx)

	case v1alpha1.PowerOff:
		if x.node != nil {
			if ready && x.since() > 30*time.Second {
				x.transition(v1alpha1.PowerOn, "machine came up on its own")
				st.Target, st.TargetReason = v1alpha1.TargetOn, "machine came up on its own"
				return x.untaint(ctx)
			}
			// Powered on by hand: the kubelet is back but the off taint
			// (NoExecute) keeps the CNI DaemonSet away, so the node can't
			// become Ready. Treat it as booting: NoSchedule lets DaemonSets
			// start, and it goes On once Ready. Heartbeats in the first
			// minutes after Off may be a kubelet still shutting down.
			if st.LastTransitionTime != nil && KubeletPostedAfter(x.node, st.LastTransitionTime.Add(offSettle)) {
				st.Target, st.TargetReason = v1alpha1.TargetOn, "machine came up on its own"
				st.PowerOnAttempt = 0
				x.transition(v1alpha1.PowerBooting, "kubelet is posting status again; machine came up on its own")
				return x.bootTaint(ctx)
			}
			if err := setTaint(ctx, x.s.Client, x.node, corev1.TaintEffectNoExecute); err != nil {
				return err
			}
			if !ready {
				if err := x.clearStrandedPods(ctx); err != nil {
					return err
				}
			}
		}
		if st.Target == v1alpha1.TargetOn {
			if len(x.m.Spec.PowerOn) == 0 {
				x.transition(v1alpha1.PowerFailed, "no powerOn drivers configured")
				return nil
			}
			st.PowerOnAttempt = 0
			x.transition(v1alpha1.PowerBooting, "powering on")
			if err := x.bootTaint(ctx); err != nil {
				return err
			}
			return x.powerOn(ctx)
		}
		return nil

	case v1alpha1.PowerBooting:
		if err := x.bootTaint(ctx); err != nil {
			return err
		}
		if ready {
			boot := x.since()
			st.FailureCount, st.PowerOnAttempt = 0, 0
			x.s.recordWake(boot)
			x.transition(v1alpha1.PowerOn, fmt.Sprintf("Ready after %s", boot.Round(time.Second)))
			return x.untaint(ctx)
		}
		if x.since() > x.to.boot {
			st.PowerOnAttempt++
			if int(st.PowerOnAttempt) >= len(x.m.Spec.PowerOn) {
				st.FailureCount++
				x.s.event(x.m, corev1.EventTypeWarning, "BootFailed", "node not Ready after trying %d driver(s)", len(x.m.Spec.PowerOn))
				x.s.recordFailedBoot()
				x.transition(v1alpha1.PowerFailed, "node did not become Ready")
				return nil
			}
			st.LastTransitionTime = &metav1.Time{Time: x.s.now()}
		}
		// Re-send every loop: WoL packets get lost; plugs and BMCs are idempotent.
		return x.powerOn(ctx)

	case v1alpha1.PowerFailed:
		if ready {
			x.transition(v1alpha1.PowerOn, "node is Ready")
			return x.untaint(ctx)
		}
		if st.Target == v1alpha1.TargetOff || x.since() > failureBackoff(st.FailureCount) {
			x.transition(v1alpha1.PowerOff, "retrying after failure")
		}
		return nil
	}
	return fmt.Errorf("unknown power state %q", st.PowerState)
}

// bootTaint relaxes the off taint to NoSchedule while booting. DaemonSets
// such as the CNI (flannel tolerates NoSchedule but not NoExecute) must start
// for the Node to become Ready; ordinary pods still wait until it is.
func (x *machineStep) bootTaint(ctx context.Context) error {
	if x.node == nil {
		return nil
	}
	return setTaint(ctx, x.s.Client, x.node, corev1.TaintEffectNoSchedule)
}

func failureBackoff(failures int32) time.Duration {
	return min(time.Minute*time.Duration(1<<min(failures, 6)), maxFailureBackoff)
}

func (x *machineStep) powerOn(ctx context.Context) error {
	i := int(x.m.Status.PowerOnAttempt)
	if i >= len(x.m.Spec.PowerOn) {
		return errors.New("no powerOn driver left")
	}
	d := x.m.Spec.PowerOn[i]
	if err := x.s.Drivers.PowerOn(ctx, d, x.target()); err != nil {
		return err
	}
	x.m.Status.Message = fmt.Sprintf("waiting for Ready after powerOn[%d] (%s)", i, drivers.Kind(d))
	return nil
}

func (x *machineStep) abortDrain(ctx context.Context, reason string) error {
	x.m.Status.Target, x.m.Status.TargetReason = v1alpha1.TargetOn, reason
	x.transition(v1alpha1.PowerOn, reason)
	return x.untaint(ctx)
}

func (x *machineStep) drain(ctx context.Context) error {
	if x.m.Status.Target == v1alpha1.TargetOn {
		return x.abortDrain(ctx, "power-off cancelled")
	}
	if x.node == nil {
		x.transition(v1alpha1.PowerShuttingDown, "node object missing")
		return nil
	}
	if err := setTaint(ctx, x.s.Client, x.node, corev1.TaintEffectNoSchedule); err != nil {
		return err
	}
	movable := planner.MovablePods(x.pods)
	remaining := 0
	var blocked []string
	for _, p := range movable {
		remaining++
		if p.DeletionTimestamp != nil {
			continue
		}
		if x.s.SelfPod == p.Namespace+"/"+p.Name && len(movable) > 1 {
			continue // evict ourselves last, once everything else has left
		}
		ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
		if err := x.s.Client.SubResource("eviction").Create(ctx, p, ev); err != nil {
			if apierrors.IsNotFound(err) {
				remaining--
				continue
			}
			blocked = append(blocked, p.Namespace+"/"+p.Name) // usually a PodDisruptionBudget (429)
		}
	}
	if remaining == 0 {
		return x.startShutdown(ctx)
	}
	if x.since() > x.to.drain {
		x.s.event(x.m, corev1.EventTypeWarning, "DrainTimeout", "giving up after %s; %d pod(s) left, blocked: %v", x.to.drain, remaining, blocked)
		return x.abortDrain(ctx, "drain timed out")
	}
	x.m.Status.Message = fmt.Sprintf("waiting for %d pod(s) to leave", remaining)
	return nil
}

func (x *machineStep) startShutdown(ctx context.Context) error {
	spec := x.m.Spec
	switch {
	case spec.Shutdown != nil:
		if err := x.s.Drivers.Shutdown(ctx, *spec.Shutdown, x.target()); err != nil {
			return err
		}
		x.transition(v1alpha1.PowerShuttingDown, "shutdown requested via "+drivers.Kind(*spec.Shutdown))
	case spec.PowerCut != nil:
		if err := x.s.Drivers.PowerCut(ctx, *spec.PowerCut, x.target()); err != nil {
			return err
		}
		x.transition(v1alpha1.PowerShuttingDown, "power cut (no shutdown driver configured)")
	default:
		return x.abortDrain(ctx, "no shutdown or powerCut driver configured")
	}
	return nil
}

func (x *machineStep) shuttingDown(ctx context.Context) error {
	spec := x.m.Spec
	if x.node == nil || !NodeReady(x.node) {
		if spec.PowerCut != nil && spec.Shutdown != nil {
			delay := defaultPowerCutDelay
			if spec.PowerCutDelay != nil {
				delay = spec.PowerCutDelay.Duration
			}
			if x.node != nil && x.s.now().Sub(NotReadySince(x.node)) < delay {
				return nil
			}
			if err := x.s.Drivers.PowerCut(ctx, *spec.PowerCut, x.target()); err != nil {
				return err
			}
		}
		x.m.Status.PoweredOffAt = &metav1.Time{Time: x.s.now()}
		x.transition(v1alpha1.PowerOff, "powered off")
		return nil
	}
	if x.since() <= x.to.shutdown {
		return nil
	}
	if spec.PowerCut != nil {
		x.s.event(x.m, corev1.EventTypeWarning, "ShutdownTimeout", "OS did not shut down within %s; cutting power", x.to.shutdown)
		if err := x.s.Drivers.PowerCut(ctx, *spec.PowerCut, x.target()); err != nil {
			return err
		}
		x.m.Status.PoweredOffAt = &metav1.Time{Time: x.s.now()}
		x.transition(v1alpha1.PowerOff, "power cut after shutdown timeout")
		return nil
	}
	x.m.Status.FailureCount++
	x.s.event(x.m, corev1.EventTypeWarning, "ShutdownTimeout", "node still Ready %s after shutdown request", x.to.shutdown)
	x.transition(v1alpha1.PowerFailed, "shutdown timed out")
	return nil
}

// clearStrandedPods force-deletes non-DaemonSet pods still bound to a machine
// the scaler powered off, so their controllers recreate them elsewhere. A
// node that is merely unreachable may still be running them (a second copy
// of a database is worse than waiting), so only poweredOffAt machines qualify.
func (x *machineStep) clearStrandedPods(ctx context.Context) error {
	if x.s.now().Sub(NotReadySince(x.node)) < strandedPodGrace {
		return nil
	}
	stranded := planner.MovablePods(x.pods)
	if len(stranded) == 0 {
		return nil
	}
	if x.m.Status.PoweredOffAt == nil {
		x.m.Status.Message = fmt.Sprintf("%d pod(s) bound to this NotReady node; not force-deleting because nodenap did not power it off", len(stranded))
		return nil
	}
	for _, p := range stranded {
		if err := x.s.Client.Delete(ctx, p, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("force-deleting stranded pod %s/%s: %w", p.Namespace, p.Name, err)
		}
		x.s.event(x.m, corev1.EventTypeNormal, "StrandedPodDeleted", "force-deleted %s/%s from powered-off node so it reschedules", p.Namespace, p.Name)
	}
	return nil
}
