// Command powerplan is a read-only what-if tool. It reads Nodes and Pods from
// a live cluster, takes a PowerPolicy and Machines from local YAML, and runs
// nodenap's real loop (in dry run) against an in-memory copy of the
// cluster. Nothing is written to the cluster and no machine is touched.
// NotReady Nodes stand in for powered-off machines' capability cache.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/scaler"
	"github.com/rothgar/nodenap/internal/sources"
)

const simNamespace = "nodenap"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var configFile, pendingFile, promService, offList, selfNode string
	flag.StringVar(&configFile, "f", "", "YAML with a PowerPolicy and Machines (required)")
	flag.StringVar(&pendingFile, "pending", "", "YAML with hypothetical Pods to treat as unschedulable")
	flag.StringVar(&promService, "prometheus-service", "", "reach Prometheus through the API server proxy, e.g. monitoring/prometheus-operated:9090")
	flag.StringVar(&offList, "assume-off", "", "comma-separated nodes to simulate as powered off")
	flag.StringVar(&selfNode, "self-node", "", "node nodenap would run on")
	flag.Parse()
	if configFile == "" {
		return errors.New("-f is required")
	}
	ctx := context.Background()

	kubeconfig := os.Getenv("KUBECONFIG")
	if f := flag.Lookup("kubeconfig"); f != nil && f.Value.String() != "" {
		kubeconfig = f.Value.String()
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	live, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	policy, machines, err := loadConfig(configFile)
	if err != nil {
		return err
	}
	policy.Spec.DryRun = true

	var nodes corev1.NodeList
	var pods corev1.PodList
	if err := live.List(ctx, &nodes); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	if err := live.List(ctx, &pods); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	off := map[string]bool{}
	for _, n := range strings.Split(offList, ",") {
		if n = strings.TrimSpace(n); n != "" {
			off[n] = true
		}
	}

	objs := []client.Object{policy}
	podsByNode := map[string][]*corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if off[p.Spec.NodeName] || p.DeletionTimestamp != nil {
			continue // off machines run nothing; terminating pods can't go in the fake client
		}
		podsByNode[p.Spec.NodeName] = append(podsByNode[p.Spec.NodeName], p)
		objs = append(objs, p)
	}
	byName := map[string]*corev1.Node{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if off[n.Name] {
			for j, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady {
					n.Status.Conditions[j].Status = corev1.ConditionUnknown
				}
			}
		}
		byName[n.Name] = n
		objs = append(objs, n)
	}
	now := time.Now()
	for _, m := range machines {
		n := byName[m.NodeName()]
		if n == nil {
			fmt.Fprintf(os.Stderr, "warning: Machine %s: no Node %q in the cluster\n", m.Name, m.NodeName())
		} else {
			// The scaler's view after it has been running a while.
			m.Status.CachedNode = scaler.Snapshot(n, podsByNode[n.Name], now)
			m.Status.PowerState, m.Status.Target = v1alpha1.PowerOff, v1alpha1.TargetOff
			if scaler.NodeReady(n) {
				m.Status.PowerState, m.Status.Target = v1alpha1.PowerOn, v1alpha1.TargetOn
			}
			m.Status.UnneededSince = &metav1.Time{Time: now.Add(-24 * time.Hour)}
		}
		objs = append(objs, m)
	}
	// Secrets are never read from the live cluster; HA_TOKEN goes into the copy.
	if ref := policy.Spec.Integrations.HomeAssistantTokenSecretRef; ref != nil {
		tok := os.Getenv("HA_TOKEN")
		if tok == "" {
			return errors.New("the policy uses homeAssistantTokenSecretRef: set HA_TOKEN")
		}
		key := ref.Key
		if key == "" {
			key = "token"
		}
		objs = append(objs, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: simNamespace},
			Data: map[string][]byte{key: []byte(tok)}})
	}
	if pendingFile != "" {
		extra, err := loadPending(pendingFile)
		if err != nil {
			return err
		}
		for _, p := range extra {
			objs = append(objs, p)
		}
	}

	sim := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Machine{}, &v1alpha1.PowerPolicy{}).Build()
	s := &scaler.Scaler{Client: sim, Namespace: simNamespace, SelfNode: selfNode}
	if promService != "" {
		httpClient, err := rest.HTTPClientFor(cfg)
		if err != nil {
			return err
		}
		ns, svc, ok := strings.Cut(promService, "/")
		if !ok {
			return errors.New("--prometheus-service must be namespace/service:port")
		}
		base := strings.TrimRight(cfg.Host, "/") + "/api/v1/namespaces/" + ns + "/services/" + svc + "/proxy"
		s.NewPrometheus = func(string) *sources.Prometheus { return &sources.Prometheus{BaseURL: base, HTTP: httpClient} }
	}
	if _, err := s.RunOnce(ctx); err != nil {
		return err
	}
	// Run again as if unneededTime had passed, so suggestions for nodes
	// without a Machine show up as they would on a running install.
	later := time.Now()
	if d := policy.Spec.ScaleDown.UnneededTime; d != nil && d.Duration > 0 {
		later = later.Add(d.Duration + time.Minute)
	} else {
		later = later.Add(16 * time.Minute)
	}
	s.Now = func() time.Time { return later }
	if _, err := s.RunOnce(ctx); err != nil {
		return err
	}
	return report(ctx, sim, os.Stdout)
}

func decodeAll(path string, fn func(doc []byte, kind string) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var tm metav1.TypeMeta
		if err := yaml.Unmarshal(doc, &tm); err != nil {
			return err
		}
		if tm.Kind != "" {
			if err := fn(doc, tm.Kind); err != nil {
				return err
			}
		}
	}
}

func loadConfig(path string) (*v1alpha1.PowerPolicy, []*v1alpha1.Machine, error) {
	policy := &v1alpha1.PowerPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	var machines []*v1alpha1.Machine
	err := decodeAll(path, func(doc []byte, kind string) error {
		switch kind {
		case "PowerPolicy":
			return yaml.UnmarshalStrict(doc, policy)
		case "Machine":
			m := &v1alpha1.Machine{}
			if err := yaml.UnmarshalStrict(doc, m); err != nil {
				return err
			}
			machines = append(machines, m)
		}
		return nil
	})
	return policy, machines, err
}

func loadPending(path string) ([]*corev1.Pod, error) {
	var out []*corev1.Pod
	err := decodeAll(path, func(doc []byte, kind string) error {
		if kind != "Pod" {
			return nil
		}
		p := &corev1.Pod{}
		if err := yaml.Unmarshal(doc, p); err != nil {
			return err
		}
		if p.Namespace == "" {
			p.Namespace = "default"
		}
		p.Spec.NodeName = ""
		p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		}}}
		out = append(out, p)
		return nil
	})
	return out, err
}

func report(ctx context.Context, c client.Client, w io.Writer) error {
	var policies v1alpha1.PowerPolicyList
	if err := c.List(ctx, &policies); err != nil || len(policies.Items) == 0 {
		return fmt.Errorf("reading simulated policy: %v", err)
	}
	st := policies.Items[0].Status
	if st.Error != "" {
		return errors.New(st.Error)
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	fmt.Fprintf(w, "Energy: price %s/kWh, effective %s/kWh, grid %sW (+export), battery %s%%\n",
		st.Price, st.EffectivePrice, dash(st.GridPowerWatts), dash(st.BatterySoCPercent))
	fmt.Fprintf(w, "Fleet:  %d on, %d off, %sW, %s/h, %d pending pod(s)\n\n", st.NodesOn, st.NodesOff, st.MachinesWatts, st.CostPerHour, st.PendingPods)

	var machines v1alpha1.MachineList
	if err := c.List(ctx, &machines); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tSTATE\tWATTS\tCPU\tMEMORY\tGPU\tARCH\tUNNEEDED")
	for _, m := range machines.Items {
		cpu, mem, gpu, arch := "-", "-", "-", "-"
		if c := m.Status.CachedNode; c != nil {
			if q, ok := c.Allocatable[corev1.ResourceCPU]; ok {
				cpu = q.String()
			}
			if q, ok := c.Allocatable[corev1.ResourceMemory]; ok {
				mem = fmt.Sprintf("%.0fGi", float64(q.Value())/(1<<30))
			}
			if q, ok := c.Allocatable["nvidia.com/gpu"]; ok {
				gpu = q.String()
			}
			arch = c.Labels["kubernetes.io/arch"]
		}
		unneeded := ""
		if m.Status.UnneededSince != nil {
			unneeded = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, m.Status.PowerState, dash(m.Status.MeasuredWatts), cpu, mem, gpu, arch, unneeded)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "\nDecisions:")
	if len(st.LastActions) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, a := range st.LastActions {
		fmt.Fprintf(w, "  %-4s %-10s %s\n", a.Target, a.Machine, a.Reason)
	}
	if len(st.Suggestions) > 0 {
		fmt.Fprintln(w, "\nSuggestions for nodes without a Machine:")
		for _, a := range st.Suggestions {
			fmt.Fprintf(w, "  %-4s %-10s %s\n", a.Target, a.Machine, a.Reason)
		}
	}
	for _, u := range st.UnsatisfiablePods {
		fmt.Fprintln(w, "  unsatisfiable:", u)
	}
	for _, warn := range st.Warnings {
		fmt.Fprintln(w, "  warning:", warn)
	}
	return nil
}
