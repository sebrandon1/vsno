package precheck

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

// HostMinor reads the host's version, and says nothing when it can't.
func TestHostMinor(t *testing.T) {
	cv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "config.openshift.io/v1", "kind": "ClusterVersion",
		"metadata": map[string]any{"name": "version"},
		"status":   map[string]any{"desired": map[string]any{"version": "4.22.22"}},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVRClusterVersion: "ClusterVersionList"}, cv)
	if got := HostMinor(context.Background(), dyn); got != "4.22" {
		t.Errorf("HostMinor = %q, want 4.22", got)
	}
	empty := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVRClusterVersion: "ClusterVersionList"})
	if got := HostMinor(context.Background(), empty); got != "" {
		t.Errorf("HostMinor with no ClusterVersion = %q, want \"\"", got)
	}
}
