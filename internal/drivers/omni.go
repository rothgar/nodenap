package drivers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/omni/client/api/omni/specs"
	omniclient "github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
)

// OmniAPI is the slice of the Omni API used (fakeable in tests).
type OmniAPI interface {
	MachinePowerOff(ctx context.Context, machineID string) error
	MachinePowerOn(ctx context.Context, machineID string) error
	MachineIDForNode(ctx context.Context, nodeName string) (string, error)
	// Upgrade describes a Talos or Kubernetes upgrade in progress on the
	// node's cluster, or "".
	Upgrade(ctx context.Context, nodeName string) (string, error)
	Close() error
}

// Omni shuts machines down (and powers infra-provider machines on) via Omni.
type Omni struct {
	API       OmniAPI
	MachineID string // looked up from NodeName when empty
	NodeName  string
}

func (o *Omni) machine(ctx context.Context) (string, error) {
	if o.MachineID != "" {
		return o.MachineID, nil
	}
	if o.NodeName == "" {
		return "", errors.New("omni: no machineId and no node name")
	}
	return o.API.MachineIDForNode(ctx, o.NodeName)
}

// Shutdown is a graceful Talos shutdown performed by Omni.
func (o *Omni) Shutdown(ctx context.Context) error {
	defer o.API.Close() //nolint:errcheck
	id, err := o.machine(ctx)
	if err != nil {
		return err
	}
	return o.API.MachinePowerOff(ctx, id)
}

// On only works for machines managed by an Omni infrastructure provider.
func (o *Omni) On(ctx context.Context) error {
	defer o.API.Close() //nolint:errcheck
	id, err := o.machine(ctx)
	if err != nil {
		return err
	}
	if err := o.API.MachinePowerOn(ctx, id); err != nil {
		return fmt.Errorf("only machines run by an Omni infra provider can be powered on this way; use wakeOnLAN/http/ipmi/redfish: %w", err)
	}
	return nil
}

type omniClientAPI struct{ c *omniclient.Client }

// NewOmniAPI connects to Omni with a service account key.
func NewOmniAPI(endpoint, serviceAccountKey string) (OmniAPI, error) {
	c, err := omniclient.New(endpoint, omniclient.WithServiceAccount(serviceAccountKey))
	if err != nil {
		return nil, fmt.Errorf("omni client: %w", err)
	}
	return &omniClientAPI{c: c}, nil
}

func (a *omniClientAPI) MachinePowerOff(ctx context.Context, id string) error {
	return a.c.Management().MachinePowerOff(ctx, id)
}

func (a *omniClientAPI) MachinePowerOn(ctx context.Context, id string) error {
	return a.c.Management().MachinePowerOn(ctx, id)
}

func (a *omniClientAPI) MachineIDForNode(ctx context.Context, nodeName string) (string, error) {
	ids, err := safe.StateListAll[*omni.ClusterMachineIdentity](ctx, a.c.Omni().State())
	if err != nil {
		return "", fmt.Errorf("omni: listing machines: %w", err)
	}
	for it := ids.Iterator(); it.Next(); {
		if it.Value().TypedSpec().Value.GetNodename() == nodeName {
			return it.Value().Metadata().ID(), nil
		}
	}
	return "", fmt.Errorf("omni: no machine with Kubernetes node name %q", nodeName)
}

func (a *omniClientAPI) Upgrade(ctx context.Context, nodeName string) (string, error) {
	id, err := a.MachineIDForNode(ctx, nodeName)
	if err != nil {
		return "", err
	}
	cmi, err := safe.StateGetByID[*omni.ClusterMachineIdentity](ctx, a.c.Omni().State(), id)
	if err != nil {
		return "", fmt.Errorf("omni: %w", err)
	}
	cluster, _ := cmi.Metadata().Labels().Get(omni.LabelCluster)
	if cluster == "" {
		return "", fmt.Errorf("omni: machine %s has no cluster", id)
	}
	var parts []string
	if t, err := safe.StateGetByID[*omni.TalosUpgradeStatus](ctx, a.c.Omni().State(), cluster); err == nil {
		s := t.TypedSpec().Value
		switch s.GetPhase() {
		case specs.TalosUpgradeStatusSpec_Upgrading, specs.TalosUpgradeStatusSpec_Reverting,
			specs.TalosUpgradeStatusSpec_UpdatingMachineSchematics:
			parts = append(parts, upgradeText("Talos", s.GetPhase().String(), s.GetLastUpgradeVersion(), s.GetCurrentUpgradeVersion()))
		}
	} else if !state.IsNotFoundError(err) {
		return "", fmt.Errorf("omni: talos upgrade status: %w", err)
	}
	if k, err := safe.StateGetByID[*omni.KubernetesUpgradeStatus](ctx, a.c.Omni().State(), cluster); err == nil {
		s := k.TypedSpec().Value
		switch s.GetPhase() {
		case specs.KubernetesUpgradeStatusSpec_Upgrading, specs.KubernetesUpgradeStatusSpec_Reverting:
			parts = append(parts, upgradeText("Kubernetes", s.GetPhase().String(), s.GetLastUpgradeVersion(), s.GetCurrentUpgradeVersion()))
		}
	} else if !state.IsNotFoundError(err) {
		return "", fmt.Errorf("omni: kubernetes upgrade status: %w", err)
	}
	return strings.Join(parts, ", "), nil
}

func upgradeText(what, phase, from, to string) string {
	s := "Omni " + what + " " + strings.ToLower(phase)
	if from != "" && to != "" {
		s += " " + from + " → " + to
	}
	return s
}

func (a *omniClientAPI) Close() error { return a.c.Close() }
