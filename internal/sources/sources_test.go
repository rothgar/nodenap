package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

func resolver(t *testing.T, data map[string]map[string]string) *Resolver {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	b := fake.NewClientBuilder().WithScheme(s)
	for name, kv := range data {
		d := map[string][]byte{}
		for k, v := range kv {
			d[k] = []byte(v)
		}
		b = b.WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hs"}, Data: d})
	}
	return &Resolver{Reader: b.Build(), Namespace: "hs"}
}

// A Shelly Gen2 plug behind SHA-256 digest auth.
func TestHTTPDigestAuthShellyGen2(t *testing.T) {
	h := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, p := parseChallenge(r.Header.Get("Authorization"))
		ha1 := h("admin:shelly:pw")
		ha2 := h(r.Method + ":" + r.URL.RequestURI())
		if p["response"] != h(ha1+":n1:"+p["nc"]+":"+p["cnonce"]+":auth:"+ha2) {
			w.Header().Set("WWW-Authenticate", `Digest qop="auth", realm="shelly", nonce="n1", algorithm=SHA-256`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"output":true,"apower":131.5}`)
	}))
	defer srv.Close()
	r := resolver(t, map[string]map[string]string{"plug": {"password": "pw"}})
	req := &v1alpha1.HTTPRequest{URL: srv.URL + "/rpc/Switch.GetStatus?id=0", Field: "apower",
		Auth: &v1alpha1.HTTPAuth{Type: "digest", SecretRef: v1alpha1.SecretRef{Name: "plug"}}}
	if w, err := r.ReadHTTP(context.Background(), req); err != nil || w != 131.5 {
		t.Fatalf("watts %v %v", w, err)
	}
	bad := resolver(t, map[string]map[string]string{"plug": {"password": "nope"}})
	if _, err := bad.ReadHTTP(context.Background(), req); err == nil {
		t.Fatal("wrong password must fail")
	}
}

func TestExtractNumber(t *testing.T) {
	for _, c := range []struct {
		body, path string
		want       float64
	}{
		{`{"state":"0.0381"}`, "state", 0.0381},
		{`{"meters":[{"power":1},{"power":23.5}]}`, "meters.1.power", 23.5},
		{"42.5\n", "", 42.5},
		{`{"StatusSNS":{"ENERGY":{"Power":17}}}`, "StatusSNS.ENERGY.Power", 17}, // Tasmota
	} {
		if v, err := ExtractNumber([]byte(c.body), c.path); err != nil || v != c.want {
			t.Errorf("%s %q: %v %v", c.body, c.path, v, err)
		}
	}
	for _, c := range [][2]string{{`{"a":"x"}`, "a"}, {`{"a":[1]}`, "a.3"}, {`{"a":1}`, "b"}} {
		if _, err := ExtractNumber([]byte(c[0]), c[1]); err == nil {
			t.Errorf("%s %q should fail", c[0], c[1])
		}
	}
}

func TestEvalSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/query":
			if q := r.URL.Query().Get("query"); q != `shelly_power_watts{device="shelly-nas"}` {
				t.Errorf("query %q", q)
			}
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"12.5"]}]}}`)
		case "/api/states/sensor.a", "/api/states/sensor.b":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"state":"-450"}`)
		}
	}))
	defer srv.Close()
	r := resolver(t, nil)
	r.Prom, r.HA = NewPrometheus(srv.URL), NewHomeAssistant(srv.URL, "tok")
	ctx := context.Background()
	if v, err := r.Eval(ctx, &v1alpha1.Value{Prometheus: &v1alpha1.PrometheusSource{Query: `shelly_power_watts{device="shelly-{{ .NodeName }}"}`}}, "nas"); err != nil || v != 12.5 {
		t.Fatalf("prometheus %v %v", v, err)
	}
	if v, err := r.Eval(ctx, &v1alpha1.Value{HomeAssistant: &v1alpha1.HomeAssistantSource{Entities: []string{"sensor.a", "sensor.b"}}}, ""); err != nil || v != -900 {
		t.Fatalf("home assistant sum %v %v", v, err)
	}
	if v, err := r.Eval(ctx, &v1alpha1.Value{Static: "1.5", Scale: "-1000"}, ""); err != nil || v != -1500 {
		t.Fatalf("static+scale %v %v", v, err)
	}
	if _, err := r.Eval(ctx, &v1alpha1.Value{}, ""); err == nil {
		t.Fatal("empty value must fail")
	}
}
