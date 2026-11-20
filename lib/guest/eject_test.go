package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// installed is a vSNO whose VM still carries the install ISO.
func installed(name string) *unstructured.Unstructured {
	u := vm(name, "Running")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"name": "root", "bootOrder": int64(1), "disk": map[string]any{"bus": "virtio"}},
		map[string]any{"name": "iso", "bootOrder": int64(2), "cdrom": map[string]any{"bus": "sata"}},
	}, "spec", "template", "spec", "domain", "devices", "disks")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"name": "root", "dataVolume": map[string]any{"name": name + "-root"}},
		map[string]any{"name": "iso", "dataVolume": map[string]any{"name": name + "-iso"}},
	}, "spec", "template", "spec", "volumes")
	return u
}

func TestEjectISO(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, installed("a"))
	if _, err := w.dyn.Resource(GVRDataVolume).Namespace(ns).Create(ctx, dv("a-iso", "Succeeded", "100%"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	changed, err := EjectISO(ctx, w.env(), ns, "a")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	u, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	disks, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "domain", "devices", "disks")
	vols, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "volumes")
	if len(disks) != 1 || disks[0].(map[string]any)["name"] != "root" {
		t.Errorf("disks = %v", disks)
	}
	if len(vols) != 1 || vols[0].(map[string]any)["name"] != "root" {
		t.Errorf("volumes = %v", vols)
	}
	if _, err := w.dyn.Resource(GVRDataVolume).Namespace(ns).Get(ctx, "a-iso", metav1.GetOptions{}); err == nil {
		t.Error("the ISO DataVolume is still there")
	}

	// Again: nothing left to eject.
	changed, err = EjectISO(ctx, w.env(), ns, "a")
	if err != nil || changed {
		t.Errorf("second eject: changed=%v err=%v", changed, err)
	}
}

func TestEjectISOMissing(t *testing.T) {
	if _, err := EjectISO(context.Background(), newWorld(t).env(), ns, "ghost"); err == nil || !strings.Contains(err.Error(), "no vSNO named ghost") {
		t.Errorf("got %v", err)
	}
}
