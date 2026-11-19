// Package status reports on a vSNO environment: the host components setup
// installs, the support services in the namespace, the VMs, and the stages
// recorded in the state ConfigMap. It only reads.
package status

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Component is one row of the report. Status is Pass, Warn or Fail.
type Component struct {
	Name   string          `json:"name"`
	Status precheck.Status `json:"status"`
	Detail string          `json:"detail"`
}

// VM is a vSNO virtual machine in the namespace.
type VM struct {
	Name   string `json:"name"`
	Status string `json:"status"` // KubeVirt printableStatus, e.g. Running
	Ready  bool   `json:"ready"`
	Node   string `json:"node,omitempty"`
}

// Report is the status of one environment.
type Report struct {
	Namespace string `json:"namespace"`
	// Exists is false when the namespace doesn't exist.
	Exists bool `json:"exists"`
	// Managed is true when vsno created the namespace or recorded state in it.
	Managed  bool          `json:"managed"`
	Node     string        `json:"node,omitempty"`
	Disk     string        `json:"disk,omitempty"`
	Host     []Component   `json:"host"`
	Support  []Component   `json:"support,omitempty"`
	Registry string        `json:"registry,omitempty"`
	VMs      []VM          `json:"vms,omitempty"`
	Stages   []state.Stage `json:"stages,omitempty"`
}

// Healthy reports whether no component failed.
func (r *Report) Healthy() bool {
	for _, rows := range [][]Component{r.Host, r.Support} {
		for _, c := range rows {
			if c.Status == precheck.Fail {
				return false
			}
		}
	}
	return r.Exists
}

// Collect builds the report. Problems with single components become Fail or
// Warn rows; only a failure to read the namespace or state is an error.
func Collect(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, ns string) (*Report, error) {
	r := &Report{Namespace: ns}
	nsObj, err := kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case err == nil:
		r.Exists = true
		r.Managed = cluster.ManagedByVsno(nsObj.Labels)
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	if r.Exists {
		st, found, err := state.Load(ctx, kube, ns)
		if err != nil {
			return nil, err
		}
		r.Managed = r.Managed || found
		r.Node, r.Disk, r.Stages = st.Node, st.Disk, st.Stages
	}

	r.Host = []Component{
		operator(ctx, dyn, cluster.CNV),
		operator(ctx, dyn, cluster.LVMS),
		hyperConverged(ctx, dyn),
		lvmCluster(ctx, dyn),
		storageClass(ctx, kube, r.Node),
	}
	if !r.Exists {
		return r, nil
	}
	r.Support, r.Registry = support(ctx, dyn, ns)
	r.VMs, err = vms(ctx, dyn, ns)
	if err != nil {
		r.Support = append(r.Support, Component{Name: "vms", Status: precheck.Warn, Detail: err.Error()})
	}
	return r, nil
}

func fail(name string, err error) Component {
	return Component{Name: name, Status: precheck.Fail, Detail: err.Error()}
}

func missing(name, what string) Component {
	return Component{Name: name, Status: precheck.Fail, Detail: what + " not found; run `vsno setup`"}
}

func operator(ctx context.Context, dyn dynamic.Interface, op cluster.Operator) Component {
	csvs, err := cluster.CSVs(ctx, dyn, op)
	switch {
	case apierrors.IsNotFound(err) || (err == nil && len(csvs) == 0):
		return missing(op.Key, "operator")
	case err != nil:
		return fail(op.Key, err)
	}
	c := csvs[0]
	for _, x := range csvs {
		if cluster.Phase(&x) == "Succeeded" {
			c = x
		}
	}
	phase := cluster.Phase(&c)
	st := precheck.Pass
	if phase != "Succeeded" {
		st = precheck.Warn
	}
	return Component{Name: op.Key, Status: st, Detail: c.GetName() + " " + phase}
}

func hyperConverged(ctx context.Context, dyn dynamic.Interface) Component {
	const name = "hyperconverged"
	hc, err := dyn.Resource(precheck.GVRHyperConverged).Namespace(cluster.CNVNamespace).Get(ctx, cluster.HyperConvergedName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return missing(name, "HyperConverged "+cluster.HyperConvergedName)
	case err != nil:
		return fail(name, err)
	}
	avail := cluster.Condition(hc, "Available")
	if avail != "True" {
		return Component{Name: name, Status: precheck.Warn, Detail: "Available=" + avail}
	}
	return Component{Name: name, Status: precheck.Pass, Detail: "Available"}
}

func lvmCluster(ctx context.Context, dyn dynamic.Interface) Component {
	const name = "lvmcluster"
	l, err := dyn.Resource(precheck.GVRLVMCluster).Namespace(cluster.StorageNamespace).List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsNotFound(err) || (err == nil && len(l.Items) == 0):
		return missing(name, "LVMCluster")
	case err != nil:
		return fail(name, err)
	}
	lc := l.Items[0]
	st, _, _ := unstructured.NestedString(lc.Object, "status", "state")
	if st != "Ready" {
		return Component{Name: name, Status: precheck.Warn, Detail: lc.GetName() + " " + st}
	}
	return Component{Name: name, Status: precheck.Pass, Detail: lc.GetName() + " Ready"}
}

// storageClass checks lvms-vg-vsno exists and, when the environment's node is
// known, that LVMS serves it.
func storageClass(ctx context.Context, kube kubernetes.Interface, node string) Component {
	const name = "storage"
	sc := manifests.DefaultStorageClass
	_, err := kube.StorageV1().StorageClasses().Get(ctx, sc, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return missing(name, "StorageClass "+sc)
	case err != nil:
		return fail(name, err)
	case node != "" && !precheck.HasTopoLVM(ctx, kube, node):
		return Component{Name: name, Status: precheck.Warn, Detail: "StorageClass " + sc + ", but LVMS does not serve node " + node}
	case node != "":
		return Component{Name: name, Status: precheck.Pass, Detail: "StorageClass " + sc + " on " + node}
	}
	return Component{Name: name, Status: precheck.Pass, Detail: "StorageClass " + sc}
}

// support reports each support Deployment with its Service IP, and the
// registry endpoint.
func support(ctx context.Context, dyn dynamic.Interface, ns string) ([]Component, string) {
	out := make([]Component, 0, len(cluster.Deployments))
	var registry string
	for _, n := range cluster.Deployments {
		d, err := dyn.Resource(cluster.GVRDeployment).Namespace(ns).Get(ctx, n, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			out = append(out, missing(n, "Deployment"))
			continue
		case err != nil:
			out = append(out, fail(n, err))
			continue
		}
		avail, want := cluster.Ready(d)
		c := Component{Name: n, Status: precheck.Pass, Detail: fmt.Sprintf("%d/%d ready", avail, want)}
		if !cluster.RolledOut(d) {
			c.Status = precheck.Warn
		}
		if ip := serviceIP(ctx, dyn, ns, n); ip != "" {
			c.Detail += ", " + ip
			if n == "registry" {
				registry = net.JoinHostPort(ip, strconv.Itoa(cluster.RegistryPort))
			}
		}
		out = append(out, c)
	}
	return out, registry
}

func serviceIP(ctx context.Context, dyn dynamic.Interface, ns, name string) string {
	svc, err := dyn.Resource(cluster.GVRService).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	ip, _, _ := unstructured.NestedString(svc.Object, "spec", "clusterIP")
	return ip
}

// vms lists the VirtualMachines in ns with the node their instance runs on.
// No KubeVirt CRD means no VMs.
func vms(ctx context.Context, dyn dynamic.Interface, ns string) ([]VM, error) {
	l, err := dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}
	out := make([]VM, 0, len(l.Items))
	for _, u := range l.Items {
		vm := VM{Name: u.GetName(), Ready: cluster.Condition(&u, "Ready") == "True"}
		vm.Status, _, _ = unstructured.NestedString(u.Object, "status", "printableStatus")
		if vmi, err := dyn.Resource(cluster.GVRVMI).Namespace(ns).Get(ctx, u.GetName(), metav1.GetOptions{}); err == nil {
			vm.Node, _, _ = unstructured.NestedString(vmi.Object, "status", "nodeName")
		}
		out = append(out, vm)
	}
	return out, nil
}
