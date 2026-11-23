// Package docs holds the documentation; this test keeps its examples honest.
package docs

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/drivers"
	"github.com/rothgar/nodenap/internal/energy"
)

// TestExamples strictly parses every PowerPolicy and Machine in the docs
// and samples, and checks each driver is valid in the role it's used in.
func TestExamples(t *testing.T) {
	files, _ := filepath.Glob("examples/*.yaml")
	samples, _ := filepath.Glob("../config/samples/*.yaml")
	files = append(files, samples...)
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
		for i := 0; ; i++ {
			doc, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			var tm metav1.TypeMeta
			if err := yaml.Unmarshal(doc, &tm); err != nil {
				t.Fatalf("%s doc %d: %v", f, i, err)
			}
			switch tm.Kind {
			case "PowerPolicy":
				var p v1alpha1.PowerPolicy
				if err := yaml.UnmarshalStrict(doc, &p); err != nil {
					t.Errorf("%s doc %d: %v", f, i, err)
					continue
				}
				if p.Spec.Energy.Tariff != nil {
					if _, err := energy.NewTariff(p.Spec.Energy.Tariff, time.UTC); err != nil {
						t.Errorf("%s: tariff: %v", f, err)
					}
				}
			case "Machine":
				var m v1alpha1.Machine
				if err := yaml.UnmarshalStrict(doc, &m); err != nil {
					t.Errorf("%s doc %d: %v", f, i, err)
					continue
				}
				check := func(role drivers.Role, d *v1alpha1.Driver) {
					if d != nil {
						if err := drivers.Validate(role, *d); err != nil {
							t.Errorf("%s machine %s: %v", f, m.Name, err)
						}
					}
				}
				for i := range m.Spec.PowerOn {
					check(drivers.RolePowerOn, &m.Spec.PowerOn[i])
				}
				check(drivers.RoleShutdown, m.Spec.Shutdown)
				check(drivers.RolePowerCut, m.Spec.PowerCut)
				check(drivers.RoleSensor, m.Spec.Sensor)
			}
		}
	}
}
