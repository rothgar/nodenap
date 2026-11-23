package planner

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

func cached(lbls map[string]string, taints ...corev1.Taint) *v1alpha1.CachedNode {
	alloc := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods: resource.MustParse("110"), "nvidia.com/gpu": resource.MustParse("1"),
	}
	return &v1alpha1.CachedNode{Labels: lbls, Taints: taints, Allocatable: alloc}
}

func podWith(mut func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "d"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}}
	mut(p)
	return p
}

func TestRequiredNodeAffinity(t *testing.T) {
	v := NewCachedNodeView("n", cached(map[string]string{"kubernetes.io/arch": "arm64", "zone": "garage"}))
	p := podWith(func(p *corev1.Pod) {
		p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"office"}}},
			}}},
		}}
	})
	if err := v.Check(p); err == nil || !strings.Contains(err.Error(), "affinity") {
		t.Fatalf("expected affinity mismatch, got %v", err)
	}
	p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{"garage"}
	if err := v.Check(p); err != nil {
		t.Fatal(err)
	}
}

func TestOwnTaintIgnoredOnOffNodesButOthersApply(t *testing.T) {
	c := cached(nil,
		corev1.Taint{Key: v1alpha1.TaintKey, Effect: corev1.TaintEffectNoSchedule},
		corev1.Taint{Key: "dedicated", Value: "gaming", Effect: corev1.TaintEffectPreferNoSchedule})
	v := NewCachedNodeView("n", c)
	if err := v.Check(podWith(func(*corev1.Pod) {})); err != nil {
		t.Fatalf("our taint and PreferNoSchedule must not block: %v", err)
	}
	c.Taints = append(c.Taints, corev1.Taint{Key: "gpu", Effect: corev1.TaintEffectNoExecute})
	if v := NewCachedNodeView("n", c); v.Fits(podWith(func(*corev1.Pod) {})) {
		t.Fatal("NoExecute taint should block")
	}
}

func TestExtendedResourcesAndHostPorts(t *testing.T) {
	v := NewCachedNodeView("n", cached(nil))
	gpuPod := podWith(func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		p.Spec.Containers[0].Ports = []corev1.ContainerPort{{HostPort: 8080, ContainerPort: 80}}
	})
	if err := v.Check(gpuPod); err != nil {
		t.Fatal(err)
	}
	v.Add(gpuPod)
	err := v.Check(gpuPod)
	if err == nil || !strings.Contains(err.Error(), "nvidia.com/gpu") {
		t.Fatalf("second GPU pod should not fit: %v", err)
	}
	clone := NewCachedNodeView("n", cached(nil))
	clone.HostPorts["TCP//8080"] = true
	portOnly := podWith(func(p *corev1.Pod) { p.Spec.Containers[0].Ports = []corev1.ContainerPort{{HostPort: 8080}} })
	if err := clone.Check(portOnly); err == nil || !strings.Contains(err.Error(), "host port") {
		t.Fatalf("host port conflict expected: %v", err)
	}
}

func TestDaemonSetOverheadAndInitContainers(t *testing.T) {
	c := cached(nil)
	c.DaemonSetRequests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	v := NewCachedNodeView("n", c)
	// Init container needs 3.5 cores at peak: more than the 3 left after DaemonSets.
	p := podWith(func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{Name: "i", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3500m")}}}}
	})
	if v.Fits(p) {
		t.Fatal("init container peak should not fit")
	}
}
