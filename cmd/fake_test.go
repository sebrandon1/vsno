package cmd

import (
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// testNode is the worker the fake cluster has; addVM places its vSNOs there.
const testNode = "worker-0"

// fakeClients is a cluster holding the vsno namespace and one worker, for
// command tests that only read.
func fakeClients(t *testing.T) *kube.Clients {
	t.Helper()
	return &kube.Clients{
		Config: &rest.Config{Host: "https://example.test:6443"},
		Kube: kfake.NewClientset(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNode}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("90Gi"), corev1.ResourceCPU: resource.MustParse("104")}}},
		),
		Dyn: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			cluster.GVRVirtualMachine: "VirtualMachineList",
			cluster.GVRVMI:            "VirtualMachineInstanceList",
			guest.GVRDataVolume:       "DataVolumeList",
		}),
		Context: "fake",
	}
}

// fakeEnv is the guest environment collect reads: the clients, and nothing that
// needs a real builder.
func fakeEnv(c *kube.Clients) guest.Env {
	return guest.Env{Kube: c.Kube, Dyn: c.Dyn}
}

// addVM makes a vSNO VM: version labels, a sized domain and disk, the node it
// runs on, and the status KubeVirt reports for a halted VM.
func addVM(t *testing.T, c *kube.Clients, name, version string, running bool) {
	t.Helper()
	spec := map[string]any{
		"dataVolumeTemplates": []any{map[string]any{"spec": map[string]any{"storage": map[string]any{
			"resources": map[string]any{"requests": map[string]any{"storage": "120Gi"}}}}}},
		"template": map[string]any{"spec": map[string]any{
			"nodeSelector": map[string]any{"kubernetes.io/hostname": testNode},
			"domain": map[string]any{"cpu": map[string]any{"cores": int64(8)},
				"memory": map[string]any{"guest": "16Gi"}},
		}},
	}
	if running {
		spec["running"] = true
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{
			"name": name, "namespace": namespace,
			"labels":      map[string]any{guest.LabelName: name},
			"annotations": map[string]any{guest.AnnRequested: version, guest.AnnVersion: version, guest.AnnImage: "quay.io/x:" + version},
		},
		"spec":   spec,
		"status": map[string]any{"printableStatus": "Stopped"},
	}}
	if _, err := c.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(namespace).Create(t.Context(), u, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// addBuilding makes the Service create leaves behind while it builds the ISO:
// a vSNO with no VM yet.
func addBuilding(t *testing.T, c *kube.Clients, name, version string) {
	t.Helper()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		Labels:      map[string]string{guest.LabelName: name},
		Annotations: map[string]string{guest.AnnRequested: version, guest.AnnVersion: version, guest.AnnImage: "quay.io/x:" + version},
	}}
	if _, err := c.Kube.CoreV1().Services(namespace).Create(t.Context(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}
