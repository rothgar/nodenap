// Package drivers switches machines on and off and reads their power.
//
// A v1alpha1.Driver can be placed in four roles on a Machine: powerOn,
// shutdown, powerCut and sensor. Each driver implements the roles it can:
//
//	           On  Shutdown  Cut  Watts
//	wakeOnLAN  ✓
//	http       ✓   ✓         ✓    ✓
//	ipmi       ✓   ✓(soft)   ✓    ✓
//	redfish    ✓   ✓         ✓    ✓
//	omni       ✓*  ✓
//	talos          ✓
//	ssh            ✓
//	prometheus                    ✓
package drivers

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/sources"
)

// Role is where a driver is used on a Machine.
type Role string

const (
	RolePowerOn  Role = "powerOn"
	RoleShutdown Role = "shutdown"
	RolePowerCut Role = "powerCut"
	RoleSensor   Role = "sensor"
)

var supported = map[string][]Role{
	"wakeOnLAN":  {RolePowerOn},
	"http":       {RolePowerOn, RoleShutdown, RolePowerCut, RoleSensor},
	"ipmi":       {RolePowerOn, RoleShutdown, RolePowerCut, RoleSensor},
	"redfish":    {RolePowerOn, RoleShutdown, RolePowerCut, RoleSensor},
	"omni":       {RolePowerOn, RoleShutdown},
	"talos":      {RoleShutdown},
	"ssh":        {RoleShutdown},
	"prometheus": {RoleSensor},
}

// Kind returns which driver is set, or "" when none or several are.
func Kind(d v1alpha1.Driver) string {
	var kinds []string
	for k, set := range map[string]bool{
		"wakeOnLAN": d.WakeOnLAN != nil, "http": d.HTTP != nil, "ipmi": d.IPMI != nil,
		"redfish": d.Redfish != nil, "omni": d.Omni != nil, "talos": d.Talos != nil,
		"ssh": d.SSH != nil, "prometheus": d.Prometheus != nil,
	} {
		if set {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// Validate checks a driver is set exactly once and usable in role.
func Validate(role Role, d v1alpha1.Driver) error {
	k := Kind(d)
	if k == "" {
		return fmt.Errorf("%s: set exactly one driver", role)
	}
	for _, r := range supported[k] {
		if r == role {
			return nil
		}
	}
	return fmt.Errorf("%s: the %s driver can't be used here (it supports %v)", role, k, supported[k])
}

// Machine identifies what a driver acts on.
type Machine struct {
	NodeName string
	Node     *corev1.Node // may be nil (node object missing)
}

// Env builds drivers.
type Env struct {
	*sources.Resolver
	// NewOmni connects to Omni; defaults to NewOmniAPI (overridable in tests).
	NewOmni func(endpoint, serviceAccountKey string) (OmniAPI, error)
}

// PowerOn switches the machine on.
func (e *Env) PowerOn(ctx context.Context, d v1alpha1.Driver, m Machine) error {
	return e.act(ctx, RolePowerOn, d, m)
}

// Shutdown asks the OS to shut down cleanly.
func (e *Env) Shutdown(ctx context.Context, d v1alpha1.Driver, m Machine) error {
	return e.act(ctx, RoleShutdown, d, m)
}

// PowerCut cuts power.
func (e *Env) PowerCut(ctx context.Context, d v1alpha1.Driver, m Machine) error {
	return e.act(ctx, RolePowerCut, d, m)
}

// Watts reads the machine's power draw.
func (e *Env) Watts(ctx context.Context, d v1alpha1.Driver, m Machine) (float64, error) {
	if err := Validate(RoleSensor, d); err != nil {
		return 0, err
	}
	switch {
	case d.Prometheus != nil:
		return e.Query(ctx, d.Prometheus.Query, m.NodeName)
	case d.HTTP != nil:
		return e.ReadHTTP(ctx, d.HTTP)
	case d.IPMI != nil:
		i, err := e.ipmi(ctx, d.IPMI)
		if err != nil {
			return 0, err
		}
		return i.Watts(ctx)
	case d.Redfish != nil:
		r, err := e.redfish(ctx, d.Redfish)
		if err != nil {
			return 0, err
		}
		return r.Watts(ctx)
	}
	return 0, errors.New("unreachable")
}

func (e *Env) act(ctx context.Context, role Role, d v1alpha1.Driver, m Machine) error {
	if err := Validate(role, d); err != nil {
		return err
	}
	name := Kind(d)
	err := func() error {
		switch {
		case d.WakeOnLAN != nil:
			w, err := NewWakeOnLAN(d.WakeOnLAN.MAC, d.WakeOnLAN.Broadcast, int(d.WakeOnLAN.Port))
			if err != nil {
				return err
			}
			return w.On(ctx)
		case d.HTTP != nil:
			c, err := e.HTTPCall(ctx, d.HTTP, "POST")
			if err != nil {
				return err
			}
			_, err = c.Do(ctx)
			return err
		case d.IPMI != nil:
			i, err := e.ipmi(ctx, d.IPMI)
			if err != nil {
				return err
			}
			return pick(role, i.On, i.Shutdown, i.Cut)(ctx)
		case d.Redfish != nil:
			r, err := e.redfish(ctx, d.Redfish)
			if err != nil {
				return err
			}
			return pick(role, r.On, r.Shutdown, r.Cut)(ctx)
		case d.Omni != nil:
			o, err := e.omni(ctx, d.Omni, m.NodeName)
			if err != nil {
				return err
			}
			return pick(role, o.On, o.Shutdown, nil)(ctx)
		case d.Talos != nil:
			cfg, err := e.SecretKey(ctx, d.Talos.TalosconfigSecretRef, "talosconfig")
			if err != nil {
				return err
			}
			addr := d.Talos.Endpoint
			if addr == "" {
				addr = InternalIP(m.Node)
			}
			if addr == "" {
				return errors.New("talos: no endpoint and the node has no InternalIP")
			}
			return TalosShutdown(ctx, []byte(cfg), addr)
		case d.SSH != nil:
			return e.ssh(ctx, d.SSH, m.Node)
		}
		return errors.New("unreachable")
	}()
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, role, err)
	}
	return nil
}

func pick(role Role, on, shutdown, cut func(context.Context) error) func(context.Context) error {
	switch role {
	case RolePowerOn:
		return on
	case RoleShutdown:
		return shutdown
	default:
		return cut
	}
}

func (e *Env) ipmi(ctx context.Context, s *v1alpha1.IPMI) (*IPMI, error) {
	user, pass, err := e.Credentials(ctx, s.CredentialsSecretRef)
	if err != nil {
		return nil, err
	}
	return &IPMI{Host: s.Address, Port: int(s.Port), Username: user, Password: pass}, nil
}

func (e *Env) redfish(ctx context.Context, s *v1alpha1.Redfish) (*Redfish, error) {
	user, pass, err := e.Credentials(ctx, s.CredentialsSecretRef)
	if err != nil {
		return nil, err
	}
	return &Redfish{Base: s.Address, SystemID: s.SystemID, ChassisID: s.ChassisID,
		HTTP: sources.HTTPCall{AuthType: "basic", Username: user, Password: pass, Insecure: s.InsecureSkipTLSVerify}}, nil
}

func (e *Env) omni(ctx context.Context, s *v1alpha1.Omni, nodeName string) (*Omni, error) {
	data, err := e.Secret(ctx, s.CredentialsSecretRef.Name, s.CredentialsSecretRef.Namespace)
	if err != nil {
		return nil, err
	}
	endpoint, key := string(data["endpoint"]), string(data["serviceAccountKey"])
	if endpoint == "" || key == "" {
		return nil, fmt.Errorf("secret %s needs keys \"endpoint\" and \"serviceAccountKey\"", s.CredentialsSecretRef.Name)
	}
	newOmni := e.NewOmni
	if newOmni == nil {
		newOmni = NewOmniAPI
	}
	api, err := newOmni(endpoint, key)
	if err != nil {
		return nil, err
	}
	return &Omni{API: api, MachineID: s.MachineID, NodeName: nodeName}, nil
}

// InternalIP returns the node's first InternalIP.
func InternalIP(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}
