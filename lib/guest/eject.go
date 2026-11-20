package guest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sebrandon1/vsno/lib/cluster"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// EjectISO removes the install ISO from a vSNO once it has installed: the
// CD-ROM and its volume from the VM, and the <name>-iso DataVolume. The vSNO
// boots from its root disk, so the ISO is only needed for the install. It
// reports whether the VM changed.
func EjectISO(ctx context.Context, e Env, ns, name string) (bool, error) {
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, fmt.Errorf("no vSNO named %s in %s (see `vsno list`)", name, ns)
	}
	if err != nil {
		return false, fmt.Errorf("reading vSNO %s: %w", name, err)
	}
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	nd, nv := withoutNamed(disks, "iso"), withoutNamed(vols, "iso")
	changed := len(nd) != len(disks) || len(nv) != len(vols)
	if changed {
		patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"domain":  map[string]any{"devices": map[string]any{"disks": nd}},
			"volumes": nv,
		}}}})
		if err != nil {
			return false, err
		}
		if _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return false, fmt.Errorf("ejecting the ISO from %s: %w", name, err)
		}
	}
	// The DataVolume goes either way: it is dead weight once the install ran.
	err = e.Dyn.Resource(GVRDataVolume).Namespace(ns).Delete(ctx, name+"-iso", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return changed, fmt.Errorf("deleting the ISO DataVolume of %s: %w", name, err)
	}
	return changed, nil
}

// withoutNamed returns items without the ones named name.
func withoutNamed(items []any, name string) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == name {
			continue
		}
		out = append(out, it)
	}
	return out
}
