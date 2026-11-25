package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// uidVM is the vm helper with the UID the API server would assign.
func uidVM(uid string) *unstructured.Unstructured {
	u := vm("seed", "Running")
	u.Object["metadata"].(map[string]any)["uid"] = uid
	return u
}

func owners(t *testing.T, w *world, gvr schema.GroupVersionResource, name string) []metav1.OwnerReference {
	t.Helper()
	u, err := w.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return u.GetOwnerReferences()
}

func pvcOwners(t *testing.T, w *world, name string) []metav1.OwnerReference {
	t.Helper()
	pvc, err := w.kube.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return pvc.OwnerReferences
}

func spec() Spec { return Spec{Namespace: ns, Name: "seed"} }

func TestOwnMakesChildrenCollectWithTheVM(t *testing.T) {
	w := newWorld(t, uidVM("vm-1"), dv("seed-iso", "Succeeded", ""), dv("seed-root", "Succeeded", ""), svcObj())
	if err := Own(context.Background(), w.env(), spec()); err != nil {
		t.Fatal(err)
	}
	for _, gvr := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{{cluster.GVRService, "seed"}, {GVRDataVolume, "seed-iso"}} {
		refs := owners(t, w, gvr.gvr, gvr.name)
		if len(refs) != 1 || refs[0].UID != "vm-1" || refs[0].Kind != "VirtualMachine" || refs[0].Controller != nil || refs[0].BlockOwnerDeletion != nil {
			t.Errorf("%s/%s refs = %+v", gvr.gvr.Resource, gvr.name, refs)
		}
	}
	if refs := owners(t, w, GVRDataVolume, "seed-root"); len(refs) != 0 {
		t.Errorf("the root DataVolume belongs to KubeVirt, got %+v", refs)
	}
	// Idempotent: a second call finds what it wants and patches nothing.
	if err := Own(context.Background(), w.env(), spec()); err != nil {
		t.Fatal(err)
	}
	if refs := owners(t, w, cluster.GVRService, "seed"); len(refs) != 1 {
		t.Errorf("second call changed the Service's refs: %+v", refs)
	}
}

func TestOwnReplacesAStaleReference(t *testing.T) {
	stale := svcObj()
	stale.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: "seed", UID: "old-vm"}})
	w := newWorld(t, uidVM("vm-1"), stale)
	if err := Own(context.Background(), w.env(), spec()); err != nil {
		t.Fatal(err)
	}
	refs := owners(t, w, cluster.GVRService, "seed")
	if len(refs) != 1 || refs[0].UID != "vm-1" {
		t.Errorf("refs = %+v", refs)
	}
}

func TestOwnRestoredRootPVC(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "seed-root"}}
	w := newWorld(t, uidVM("vm-1"), svcObj())
	if _, err := w.kube.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s := spec()
	s.Snapshot = "snap"
	if err := Own(context.Background(), w.env(), s); err != nil {
		t.Fatal(err)
	}
	if refs := pvcOwners(t, w, "seed-root"); len(refs) != 1 || refs[0].UID != "vm-1" {
		t.Errorf("PVC refs = %+v", refs)
	}
}

func TestOwnWithoutAVM(t *testing.T) {
	w := newWorld(t, svcObj())
	err := Own(context.Background(), w.env(), spec())
	if err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("want a not-found error, got %v", err)
	}
}

func TestOwnWithoutAUID(t *testing.T) {
	w := newWorld(t, uidVM(""), svcObj())
	if err := Own(context.Background(), w.env(), spec()); err == nil || !strings.Contains(err.Error(), "no UID") {
		t.Errorf("want a no-UID error, got %v", err)
	}
}

// ClearStaleOwners removes only the reference to the vSNO's VM: another
// manager's stays.
func TestClearStaleOwners(t *testing.T) {
	s := svcObj()
	s.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: "seed", UID: "dead"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: types.UID("keep")},
	})
	w := newWorld(t, s)
	if err := ClearStaleOwners(context.Background(), w.env(), spec()); err != nil {
		t.Fatal(err)
	}
	refs := owners(t, w, cluster.GVRService, "seed")
	if len(refs) != 1 || refs[0].Name != "other" {
		t.Errorf("refs = %+v", refs)
	}
}

// A JSON merge patch needs the array itself to be empty ([]), not null, or the
// reference is left where it was.
func TestClearStaleOwnersDropsTheOnlyReference(t *testing.T) {
	s := svcObj()
	s.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: "seed", UID: "dead"}})
	w := newWorld(t, s)
	if err := ClearStaleOwners(context.Background(), w.env(), spec()); err != nil {
		t.Fatal(err)
	}
	if refs := owners(t, w, cluster.GVRService, "seed"); len(refs) != 0 {
		t.Errorf("refs = %+v", refs)
	}
}

func TestClearStaleOwnersLeavesAFreshService(t *testing.T) {
	w := newWorld(t, svcObj())
	if err := ClearStaleOwners(context.Background(), w.env(), spec()); err != nil {
		t.Fatal(err)
	}
	if refs := owners(t, w, cluster.GVRService, "seed"); len(refs) != 0 {
		t.Errorf("refs = %+v", refs)
	}
}
