package cluster

import (
	"context"
	"fmt"

	"github.com/sebrandon1/vsno/lib/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// DensityManager is the server-side apply field manager vsno writes density
// settings with. Setup applies HyperConverged as manager "vsno" with an empty
// spec; under that manager, the next setup would remove these fields again.
const DensityManager = "vsno-density"

// hostnameLabel is the node label KSM's nodeLabelSelector matches.
const hostnameLabel = "kubernetes.io/hostname"

// ReadDensity reads HyperConverged's density settings; without
// HyperConverged, nothing is set.
func ReadDensity(ctx context.Context, dyn dynamic.Interface) (state.Density, error) {
	hc, err := dyn.Resource(GVRHyperConverged).Namespace(CNVNamespace).Get(ctx, HyperConvergedName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return state.Density{}, nil
	}
	if err != nil {
		return state.Density{}, fmt.Errorf("reading HyperConverged: %w", err)
	}
	var d state.Density
	if pct, found, _ := unstructured.NestedInt64(hc.Object, "spec", "higherWorkloadDensity", "memoryOvercommitPercentage"); found {
		d.OvercommitPercent = int(pct)
	}
	if _, found, _ := unstructured.NestedMap(hc.Object, "spec", "ksmConfiguration"); found {
		d.KSMNode, _, _ = unstructured.NestedString(hc.Object, "spec", "ksmConfiguration", "nodeLabelSelector", "matchLabels", hostnameLabel)
		if d.KSMNode == "" {
			d.KSMNode = state.KSMEveryNode
		}
	}
	return d, nil
}

// ApplyDensity sets HyperConverged's density settings to d, as manager
// DensityManager. What d leaves unset, the manager gives up, so applying the
// previous settings puts them back.
func ApplyDensity(ctx context.Context, dyn dynamic.Interface, d state.Density) error {
	spec := map[string]any{}
	if d.OvercommitPercent > 0 {
		spec["higherWorkloadDensity"] = map[string]any{"memoryOvercommitPercentage": int64(d.OvercommitPercent)}
	}
	switch d.KSMNode {
	case "":
	case state.KSMEveryNode:
		spec["ksmConfiguration"] = map[string]any{}
	default:
		spec["ksmConfiguration"] = map[string]any{"nodeLabelSelector": map[string]any{"matchLabels": map[string]any{hostnameLabel: d.KSMNode}}}
	}
	hc := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	hc.SetAPIVersion("hco.kubevirt.io/v1beta1")
	hc.SetKind("HyperConverged")
	hc.SetNamespace(CNVNamespace)
	hc.SetName(HyperConvergedName)
	if _, err := dyn.Resource(GVRHyperConverged).Namespace(CNVNamespace).Apply(ctx, HyperConvergedName, hc,
		metav1.ApplyOptions{FieldManager: DensityManager, Force: true}); err != nil {
		return fmt.Errorf("setting HyperConverged's density: %w", err)
	}
	return nil
}

// RequestGiB is the memory a VM of guestGiB requests at percent overcommit,
// rounded up: KubeVirt divides the guest's memory by percent/100.
func RequestGiB(guestGiB int64, percent int) int64 {
	if percent <= 100 {
		return guestGiB
	}
	return (guestGiB*100 + int64(percent) - 1) / int64(percent)
}
