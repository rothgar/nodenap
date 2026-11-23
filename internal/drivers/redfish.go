package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rothgar/nodenap/internal/sources"
)

// Redfish controls a machine through its BMC's Redfish API.
type Redfish struct {
	Base, SystemID, ChassisID string
	HTTP                      sources.HTTPCall // auth/TLS; URL and method set per call
}

func (r *Redfish) call(ctx context.Context, method, path, body string) ([]byte, error) {
	c := r.HTTP
	c.Method, c.Body, c.URL = method, body, path
	if !strings.HasPrefix(path, "http") {
		c.URL = strings.TrimRight(r.Base, "/") + path
	}
	c.Headers = map[string]string{"Accept": "application/json"}
	return c.Do(ctx)
}

func (r *Redfish) get(ctx context.Context, path string, out any) error {
	b, err := r.call(ctx, "GET", path, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// member resolves /redfish/v1/<collection>/<id>, defaulting to the first member.
func (r *Redfish) member(ctx context.Context, collection, id string) (string, error) {
	if id != "" {
		return "/redfish/v1/" + collection + "/" + id, nil
	}
	var c struct {
		Members []struct {
			ID string `json:"@odata.id"`
		}
	}
	if err := r.get(ctx, "/redfish/v1/"+collection, &c); err != nil {
		return "", err
	}
	if len(c.Members) == 0 {
		return "", fmt.Errorf("redfish: no %s on %s", collection, r.Base)
	}
	return c.Members[0].ID, nil
}

func (r *Redfish) reset(ctx context.Context, resetType string) error {
	sys, err := r.member(ctx, "Systems", r.SystemID)
	if err != nil {
		return err
	}
	var s struct {
		Actions struct {
			Reset struct {
				Target string `json:"target"`
			} `json:"#ComputerSystem.Reset"`
		}
	}
	if err := r.get(ctx, sys, &s); err != nil {
		return err
	}
	target := s.Actions.Reset.Target
	if target == "" {
		target = sys + "/Actions/ComputerSystem.Reset"
	}
	_, err = r.call(ctx, "POST", target, fmt.Sprintf(`{"ResetType":%q}`, resetType))
	return err
}

func (r *Redfish) On(ctx context.Context) error       { return r.reset(ctx, "On") }
func (r *Redfish) Shutdown(ctx context.Context) error { return r.reset(ctx, "GracefulShutdown") }
func (r *Redfish) Cut(ctx context.Context) error      { return r.reset(ctx, "ForceOff") }

// Watts reads chassis power from the classic Power resource or the newer
// EnvironmentMetrics resource.
func (r *Redfish) Watts(ctx context.Context) (float64, error) {
	ch, err := r.member(ctx, "Chassis", r.ChassisID)
	if err != nil {
		return 0, err
	}
	var p struct {
		PowerControl []struct{ PowerConsumedWatts *float64 }
	}
	if r.get(ctx, ch+"/Power", &p) == nil {
		for _, pc := range p.PowerControl {
			if pc.PowerConsumedWatts != nil {
				return *pc.PowerConsumedWatts, nil
			}
		}
	}
	var em struct {
		PowerWatts *struct{ Reading *float64 }
	}
	if r.get(ctx, ch+"/EnvironmentMetrics", &em) == nil && em.PowerWatts != nil && em.PowerWatts.Reading != nil {
		return *em.PowerWatts.Reading, nil
	}
	return 0, errors.New("redfish: chassis reports no power reading")
}
