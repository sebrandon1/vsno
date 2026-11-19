// Package cluster names the host-cluster objects vsno installs or reads and
// holds small helpers to inspect them. setup, status and teardown share it.
package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/sebrandon1/vsno/lib/state"
	"k8s.io/client-go/kubernetes"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Names of the host objects setup creates or reuses.
const (
	CNVNamespace       = "openshift-cnv"
	StorageNamespace   = "openshift-storage"
	HyperConvergedName = "kubevirt-hyperconverged"
	LVMClusterName     = "vsno-lvmcluster"
	// OSImagesNamespace holds the boot-source PVCs OpenShift Virtualization imports.
	OSImagesNamespace = "openshift-virtualization-os-images"
	// TopoLVM is the LVMS CSI provisioner.
	TopoLVM = "topolvm.io"
	// ManagedByLabel marks every object vsno creates.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// MarketplaceNamespace holds the PackageManifests OLM publishes the
	// operators' channels in.
	MarketplaceNamespace = "openshift-marketplace"
)

// ManagedByVsno reports whether labels carry ManagedByLabel=vsno.
func ManagedByVsno(labels map[string]string) bool { return labels[ManagedByLabel] == "vsno" }

// CreatedByVsno reports whether vsno created namespace ns, given its labels:
// it carries ManagedByLabel=vsno, or it holds a vsno-state ConfigMap (even an
// unreadable one). setup refuses, and teardown blocks on, a namespace that
// fails this, unless --force.
func CreatedByVsno(ctx context.Context, kube kubernetes.Interface, ns string, labels map[string]string) (bool, error) {
	if ManagedByVsno(labels) {
		return true, nil
	}
	_, found, err := state.Load(ctx, kube, ns)
	if !found && err != nil {
		return false, err
	}
	return found, nil
}

// GVRs of the resources setup, status, teardown and precheck use.
var (
	GVRNamespace       = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	GVRSecret          = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	GVRService         = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	GVRDeployment      = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	GVRSubscription    = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "subscriptions"}
	GVROperatorGroup   = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1", Resource: "operatorgroups"}
	GVRInstallPlan     = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "installplans"}
	GVRCSV             = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "clusterserviceversions"}
	GVRPackageManifest = schema.GroupVersionResource{Group: "packages.operators.coreos.com", Version: "v1", Resource: "packagemanifests"}
	GVRVirtualMachine  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	GVRVMI             = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

// Deployments are the support Deployments (and Services) in the vsno namespace.
var Deployments = []string{"dnsmasq", "builder", "s3", "registry"}

// RegistryPort is the in-cluster registry's Service port.
const RegistryPort = 5000

// Operator is an OLM operator vsno installs.
type Operator struct {
	Key       string // short name, also the state Versions key
	Namespace string
	Package   string // OLM package (Subscription spec.name)
	CSVPrefix string
}

var (
	CNV  = Operator{"cnv", CNVNamespace, "kubevirt-hyperconverged", "kubevirt-hyperconverged-operator."}
	LVMS = Operator{"lvms", StorageNamespace, "lvms-operator", "lvms-operator."}
)

// CSVs lists op's ClusterServiceVersions.
func CSVs(ctx context.Context, dyn dynamic.Interface, op Operator) ([]unstructured.Unstructured, error) {
	l, err := dyn.Resource(GVRCSV).Namespace(op.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []unstructured.Unstructured
	for _, c := range l.Items {
		if strings.HasPrefix(c.GetName(), op.CSVPrefix) {
			out = append(out, c)
		}
	}
	return out, nil
}

// SucceededCSV returns the name of op's Succeeded CSV, or "".
func SucceededCSV(ctx context.Context, dyn dynamic.Interface, op Operator) (string, error) {
	csvs, err := CSVs(ctx, dyn, op)
	for _, c := range csvs {
		if Phase(&c) == "Succeeded" {
			return c.GetName(), nil
		}
	}
	return "", err
}

// Phase returns a CSV's status.phase.
func Phase(u *unstructured.Unstructured) string {
	p, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	return p
}

// CSVPhase returns the phase of op's CSV name, or "" when it doesn't exist yet.
// An upgrade waits on this rather than on SucceededCSV, which keeps returning
// the old CSV until OLM deletes it.
func CSVPhase(ctx context.Context, dyn dynamic.Interface, op Operator, name string) (string, error) {
	u, err := dyn.Resource(GVRCSV).Namespace(op.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading CSV %s/%s: %w", op.Namespace, name, err)
	}
	return Phase(u), nil
}

// Condition returns the status of condition t, or "Unknown".
func Condition(u *unstructured.Unstructured, t string) string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == t {
			if s, ok := m["status"].(string); ok {
				return s
			}
		}
	}
	return "Unknown"
}

// RolledOut reports whether the current spec of Deployment d is fully available.
func RolledOut(d *unstructured.Unstructured) bool {
	available, want := Ready(d)
	observed, _, _ := unstructured.NestedInt64(d.Object, "status", "observedGeneration")
	updated, _, _ := unstructured.NestedInt64(d.Object, "status", "updatedReplicas")
	return observed >= d.GetGeneration() && updated >= want && available >= want
}

// Ready returns the available and wanted replicas of Deployment d.
func Ready(d *unstructured.Unstructured) (available, want int64) {
	want, found, _ := unstructured.NestedInt64(d.Object, "spec", "replicas")
	if !found {
		want = 1
	}
	available, _, _ = unstructured.NestedInt64(d.Object, "status", "availableReplicas")
	return available, want
}
