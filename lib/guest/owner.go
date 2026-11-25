package guest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sebrandon1/vsno/lib/cluster"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// OwnerFieldManager writes owner references. It is deliberately not apply's
// "vsno" manager: an owner reference set through that manager would be pruned
// by the next server-side apply of the rendered template, which never carries
// one (it can't: the VM's UID doesn't exist at render time).
const OwnerFieldManager = "vsno-owner"

// ownTarget is one object a vSNO consists of that isn't made by another
// controller.
type ownTarget struct {
	kind, name string
	gvr        schema.GroupVersionResource
	pvc        bool // read and patched through the typed client
}

// preVMTargets exist before the VM does, so create can find one left by an
// earlier attempt that died.
func preVMTargets(s Spec) []ownTarget {
	out := []ownTarget{{kind: "Service", name: s.Name, gvr: cluster.GVRService}}
	if s.Snapshot != "" {
		out = append(out, ownTarget{kind: KindPVC, name: s.Name + "-root", gvr: GVRPersistentVolumeClaim, pvc: true})
	}
	return out
}

// ownedTargets are the objects vsno made and nothing else owns. The root
// DataVolume of an install, and the PVC CDI fills it into, belong to KubeVirt
// and CDI; a second owner on them would only fight those controllers.
func ownedTargets(s Spec) []ownTarget {
	out := preVMTargets(s)
	if s.Snapshot == "" {
		out = append(out, ownTarget{kind: "DataVolume", name: s.Name + "-iso", gvr: GVRDataVolume})
	}
	return out
}

// Own makes a vSNO's own objects garbage-collect with its VM: the Service, the
// ISO DataVolume, and the root PVC a snapshot restore made. Deleting the VM
// out-of-band then leaves nothing behind but the DNS entry and the builder's
// files, which no owner reference can reach.
func Own(ctx context.Context, e Env, s Spec) error {
	owner, err := vmOwner(ctx, e, s.Namespace, s.Name)
	if err != nil {
		return err
	}
	for _, t := range ownedTargets(s) {
		refs, found, err := targetOwners(ctx, e, s.Namespace, t)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		want := mergeOwners(refs, owner)
		if sameOwners(refs, want) {
			continue
		}
		if err := patchOwners(ctx, e, s.Namespace, t, want); err != nil {
			return err
		}
	}
	return nil
}

// ClearStaleOwners removes references to the vSNO's VM from the objects that
// exist before it. A create that died after making the Service leaves a
// reference to a VM that is gone; the garbage collector would then delete the
// Service a fresh create is reusing, in the middle of its ISO build. There is
// no live VM at this point: prepare refuses a name whose VM exists.
func ClearStaleOwners(ctx context.Context, e Env, s Spec) error {
	for _, t := range preVMTargets(s) {
		refs, found, err := targetOwners(ctx, e, s.Namespace, t)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		kept := dropVMRefs(refs, s.Name)
		if len(kept) == len(refs) {
			continue
		}
		if err := patchOwners(ctx, e, s.Namespace, t, kept); err != nil {
			return err
		}
	}
	return nil
}

// vmOwner is the reference everything of a vSNO hangs off.
func vmOwner(ctx context.Context, e Env, ns, name string) (metav1.OwnerReference, error) {
	u, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("reading VM %s/%s: %w", ns, name, err)
	}
	if u.GetUID() == "" {
		return metav1.OwnerReference{}, fmt.Errorf("VM %s/%s has no UID yet", ns, name)
	}
	// controller and blockOwnerDeletion stay unset: neither is needed for
	// garbage collection, and blockOwnerDeletion needs update on the owner's
	// finalizers, which a namespace-scoped editor may not have.
	return metav1.OwnerReference{
		APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Name: u.GetName(), UID: u.GetUID(),
	}, nil
}

func targetOwners(ctx context.Context, e Env, ns string, t ownTarget) ([]metav1.OwnerReference, bool, error) {
	if t.pvc {
		pvc, err := e.Kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, t.name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("reading %s %s: %w", t.kind, t.name, err)
		}
		return pvc.OwnerReferences, true, nil
	}
	u, err := e.Dyn.Resource(t.gvr).Namespace(ns).Get(ctx, t.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("reading %s %s: %w", t.kind, t.name, err)
	}
	return u.GetOwnerReferences(), true, nil
}

func patchOwners(ctx context.Context, e Env, ns string, t ownTarget, refs []metav1.OwnerReference) error {
	if refs == nil {
		refs = []metav1.OwnerReference{} // a JSON merge patch needs "[]", not "null"
	}
	b, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": refs}})
	if err != nil {
		return err
	}
	opts := metav1.PatchOptions{FieldManager: OwnerFieldManager}
	if t.pvc {
		_, err = e.Kube.CoreV1().PersistentVolumeClaims(ns).Patch(ctx, t.name, types.MergePatchType, b, opts)
	} else {
		_, err = e.Dyn.Resource(t.gvr).Namespace(ns).Patch(ctx, t.name, types.MergePatchType, b, opts)
	}
	if err != nil {
		return fmt.Errorf("setting %s %s's owner: %w", t.kind, t.name, err)
	}
	return nil
}

// mergeOwners keeps every reference but ours, and replaces a stale reference to
// an earlier VM with the current one. Merge patch replaces the whole array, so
// the result is recomputed from a fresh read rather than appended to.
func mergeOwners(refs []metav1.OwnerReference, owner metav1.OwnerReference) []metav1.OwnerReference {
	out := make([]metav1.OwnerReference, 0, len(refs)+1)
	have := false
	for _, r := range refs {
		if ours(r, owner.Name) {
			if r.UID == owner.UID {
				out, have = append(out, r), true
			}
			continue
		}
		out = append(out, r)
	}
	if !have {
		out = append(out, owner)
	}
	return out
}

func dropVMRefs(refs []metav1.OwnerReference, vmName string) []metav1.OwnerReference {
	out := make([]metav1.OwnerReference, 0, len(refs))
	for _, r := range refs {
		if !ours(r, vmName) {
			out = append(out, r)
		}
	}
	return out
}

// ours reports whether r is a reference to the vSNO's VM.
func ours(r metav1.OwnerReference, vmName string) bool {
	return r.Kind == "VirtualMachine" && r.Name == vmName
}

func sameOwners(a, b []metav1.OwnerReference) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].APIVersion != b[i].APIVersion || a[i].Kind != b[i].Kind || a[i].Name != b[i].Name || a[i].UID != b[i].UID {
			return false
		}
	}
	return true
}
