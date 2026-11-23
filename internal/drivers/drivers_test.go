package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/sources"
)

func env(secrets map[string]map[string]string) *Env {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	b := fake.NewClientBuilder().WithScheme(s)
	for name, kv := range secrets {
		d := map[string][]byte{}
		for k, v := range kv {
			d[k] = []byte(v)
		}
		b = b.WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hs"}, Data: d})
	}
	return &Env{Resolver: &sources.Resolver{Reader: b.Build(), Namespace: "hs"}}
}

func TestValidateRoles(t *testing.T) {
	wol := v1alpha1.Driver{WakeOnLAN: &v1alpha1.WakeOnLAN{MAC: "aa:bb:cc:dd:ee:ff"}}
	if err := Validate(RolePowerOn, wol); err != nil {
		t.Fatal(err)
	}
	if err := Validate(RoleShutdown, wol); err == nil || !strings.Contains(err.Error(), "can't be used") {
		t.Fatalf("WoL can't shut down: %v", err)
	}
	if err := Validate(RoleSensor, v1alpha1.Driver{}); err == nil {
		t.Fatal("empty driver must fail")
	}
	two := v1alpha1.Driver{Talos: &v1alpha1.Talos{}, SSH: &v1alpha1.SSH{}}
	if err := Validate(RoleShutdown, two); err == nil {
		t.Fatal("two drivers must fail")
	}
}

func TestWakeOnLANMagicPacket(t *testing.T) {
	l, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e := env(nil)
	d := v1alpha1.Driver{WakeOnLAN: &v1alpha1.WakeOnLAN{MAC: "aa-bb-cc-dd-ee-ff", Broadcast: "127.0.0.1", Port: int32(l.LocalAddr().(*net.UDPAddr).Port)}}
	if err := e.PowerOn(context.Background(), d, Machine{}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 200)
	_ = l.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := l.ReadFrom(buf)
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	if err != nil || n != 102 || !bytes.Equal(buf[:6], bytes.Repeat([]byte{0xff}, 6)) || !bytes.Equal(buf[96:102], mac) {
		t.Fatalf("bad magic packet n=%d err=%v", n, err)
	}
}

func TestHTTPHomeAssistantSwitch(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
	}))
	defer srv.Close()
	e := env(map[string]map[string]string{"ha": {"Authorization": "Bearer tok"}})
	req := func(svc string) v1alpha1.Driver {
		return v1alpha1.Driver{HTTP: &v1alpha1.HTTPRequest{URL: srv.URL + "/api/services/switch/" + svc,
			Body: `{"entity_id":"switch.nas"}`, HeadersSecretRef: &v1alpha1.SecretRef{Name: "ha"}}}
	}
	ctx := context.Background()
	if err := e.PowerOn(ctx, req("turn_on"), Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := e.PowerCut(ctx, req("turn_off"), Machine{}); err != nil {
		t.Fatal(err)
	}
	want := `POST /api/services/switch/turn_on {"entity_id":"switch.nas"}` + "\n" + `POST /api/services/switch/turn_off {"entity_id":"switch.nas"}`
	if strings.Join(calls, "\n") != want {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
}

type fakeBMC struct {
	mu     sync.Mutex
	resets []string
	modern bool
}

func (b *fakeBMC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "root" || p != "calvin" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case r.URL.Path == "/redfish/v1/Systems":
		fmt.Fprint(w, `{"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`)
	case r.URL.Path == "/redfish/v1/Systems/1":
		fmt.Fprint(w, `{"Actions":{"#ComputerSystem.Reset":{"target":"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset"}}}`)
	case strings.HasSuffix(r.URL.Path, "/ComputerSystem.Reset"):
		var body struct{ ResetType string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.resets = append(b.resets, body.ResetType)
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/redfish/v1/Chassis":
		fmt.Fprint(w, `{"Members":[{"@odata.id":"/redfish/v1/Chassis/1"}]}`)
	case r.URL.Path == "/redfish/v1/Chassis/1/Power" && !b.modern:
		fmt.Fprint(w, `{"PowerControl":[{"PowerConsumedWatts":212}]}`)
	case r.URL.Path == "/redfish/v1/Chassis/1/EnvironmentMetrics" && b.modern:
		fmt.Fprint(w, `{"PowerWatts":{"Reading":187.5}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRedfishAllRoles(t *testing.T) {
	bmc := &fakeBMC{}
	srv := httptest.NewTLSServer(bmc)
	defer srv.Close()
	e := env(map[string]map[string]string{"bmc": {"username": "root", "password": "calvin"}})
	d := v1alpha1.Driver{Redfish: &v1alpha1.Redfish{Address: srv.URL, CredentialsSecretRef: v1alpha1.SecretRef{Name: "bmc"}, InsecureSkipTLSVerify: true}}
	ctx := context.Background()
	for _, f := range []func(context.Context, v1alpha1.Driver, Machine) error{e.PowerOn, e.Shutdown, e.PowerCut} {
		if err := f(ctx, d, Machine{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(bmc.resets, ","); got != "On,GracefulShutdown,ForceOff" {
		t.Fatalf("resets %s", got)
	}
	if w, err := e.Watts(ctx, d, Machine{}); err != nil || w != 212 {
		t.Fatalf("legacy power %v %v", w, err)
	}
	bmc.modern = true
	if w, err := e.Watts(ctx, d, Machine{}); err != nil || w != 187.5 {
		t.Fatalf("EnvironmentMetrics %v %v", w, err)
	}
}

type fakeOmni struct {
	off   []string
	nodes map[string]string
	onErr error
}

func (f *fakeOmni) MachinePowerOff(_ context.Context, id string) error {
	f.off = append(f.off, id)
	return nil
}
func (f *fakeOmni) MachinePowerOn(context.Context, string) error { return f.onErr }
func (f *fakeOmni) MachineIDForNode(_ context.Context, n string) (string, error) {
	if id, ok := f.nodes[n]; ok {
		return id, nil
	}
	return "", fmt.Errorf("omni: no machine with Kubernetes node name %q", n)
}
func (f *fakeOmni) Close() error { return nil }

func TestOmniShutdownByNodeName(t *testing.T) {
	api := &fakeOmni{nodes: map[string]string{"gpu-1": "c828b893"}}
	e := env(map[string]map[string]string{"omni": {"endpoint": "https://x.omni.example", "serviceAccountKey": "a2V5"}})
	e.NewOmni = func(string, string) (OmniAPI, error) { return api, nil }
	ctx := context.Background()
	d := v1alpha1.Driver{Omni: &v1alpha1.Omni{CredentialsSecretRef: v1alpha1.SecretRef{Name: "omni"}}}
	if err := e.Shutdown(ctx, d, Machine{NodeName: "gpu-1"}); err != nil || len(api.off) != 1 || api.off[0] != "c828b893" {
		t.Fatalf("shutdown %v %v", err, api.off)
	}
	api.onErr = errors.New("FailedPrecondition")
	if err := e.PowerOn(ctx, d, Machine{NodeName: "gpu-1"}); err == nil || !strings.Contains(err.Error(), "wakeOnLAN") {
		t.Fatalf("power-on error should say what to use instead: %v", err)
	}
	if err := e.Shutdown(ctx, d, Machine{NodeName: "ghost"}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown node: %v", err)
	}
	e2 := env(map[string]map[string]string{"omni": {"endpoint": "https://x"}})
	if err := e2.Shutdown(ctx, d, Machine{NodeName: "gpu-1"}); err == nil || !strings.Contains(err.Error(), "serviceAccountKey") {
		t.Fatalf("missing key: %v", err)
	}
}
