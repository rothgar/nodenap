package drivers

import (
	"context"
	"fmt"

	ipmi "github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
)

// IPMI talks IPMI v2.0 lanplus to a BMC; one session per call.
type IPMI struct {
	Host               string
	Port               int
	Username, Password string
}

func (i *IPMI) with(ctx context.Context, fn func(*ipmi.Client) error) error {
	port := i.Port
	if port == 0 {
		port = 623
	}
	c, err := ipmi.NewClient(i.Host, port, i.Username, i.Password)
	if err != nil {
		return fmt.Errorf("ipmi %s: %w", i.Host, err)
	}
	c = c.WithInterface(ipmi.InterfaceLanplus)
	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("ipmi %s connect: %w", i.Host, err)
	}
	defer c.Close(context.WithoutCancel(ctx)) //nolint:errcheck
	return fn(c)
}

func (i *IPMI) control(ctx context.Context, ctl chassis.ChassisControl) error {
	return i.with(ctx, func(c *ipmi.Client) error { _, err := c.ChassisControl(ctx, ctl); return err })
}

func (i *IPMI) On(ctx context.Context) error { return i.control(ctx, chassis.ChassisControlPowerUp) }
func (i *IPMI) Shutdown(ctx context.Context) error {
	return i.control(ctx, chassis.ChassisControlSoftShutdown)
}
func (i *IPMI) Cut(ctx context.Context) error { return i.control(ctx, chassis.ChassisControlPowerDown) }

// Watts reads the DCMI power reading.
func (i *IPMI) Watts(ctx context.Context) (float64, error) {
	var w float64
	err := i.with(ctx, func(c *ipmi.Client) error {
		r, err := c.GetDCMIPowerReading(ctx)
		if err == nil {
			w = float64(r.CurrentPower)
		}
		return err
	})
	return w, err
}
