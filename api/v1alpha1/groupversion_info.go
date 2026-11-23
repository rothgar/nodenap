// Package v1alpha1 contains the nodenap API.
// +kubebuilder:object:generate=true
// +groupName=power.nodenap.rothgar.github.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version.
	GroupVersion = schema.GroupVersion{Group: "power.nodenap.rothgar.github.io", Version: "v1alpha1"}
	// SchemeBuilder registers the types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Well-known keys.
const (
	// TaintKey marks nodes that are (being) powered off: NoSchedule while
	// draining, NoExecute once off.
	TaintKey = "nodenap.rothgar.github.io/off"
	// OverrideAnnotation on a Machine forces it "on" or "off".
	OverrideAnnotation = "nodenap.rothgar.github.io/override"
	// IgnorePodAnnotation "true" on a pod: never wake a machine for it and
	// never keep a machine on because of it.
	IgnorePodAnnotation = "nodenap.rothgar.github.io/ignore"
	// SafeToEvictAnnotation is honoured like cluster-autoscaler's.
	SafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)
