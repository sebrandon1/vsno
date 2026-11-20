package status

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	ns   = "vsno-test"
	node = "worker-1"
)

func obj(apiVersion, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	if u.Object == nil {
		u.Object = map[string]any{}
	}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func status(s map[string]any) map[string]any { return map[string]any{"status": s} }

func available(v string) map[string]any {
	return status(map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": v}}})
}

func deployment(name string, avail int64) *unstructured.Unstructured {
	return obj("apps/v1", "Deployment", ns, name, status(map[string]any{
		"updatedReplicas": int64(1), "availableReplicas": avail}))
}

func service(name, ip string) *unstructured.Unstructured {
	return obj("v1", "Service", ns, name, map[string]any{"spec": map[string]any{"clusterIP": ip}})
}

// healthy returns the objects of a fully set up environment.
func healthy() []runtime.Object {
	objs := []runtime.Object{
		obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", cluster.CNVNamespace, "kubevirt-hyperconverged-operator.v1", status(map[string]any{"phase": "Succeeded"})),
		obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", cluster.StorageNamespace, "lvms-operator.v1", status(map[string]any{"phase": "Succeeded"})),
		obj("hco.kubevirt.io/v1beta1", "HyperConverged", cluster.CNVNamespace, cluster.HyperConvergedName, available("True")),
		obj("lvm.topolvm.io/v1alpha1", "LVMCluster", cluster.StorageNamespace, cluster.LVMClusterName, status(map[string]any{"state": "Ready"})),
		obj("kubevirt.io/v1", "VirtualMachine", ns, "seed", map[string]any{"status": map[string]any{"printableStatus": "Running",
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}),
		obj("kubevirt.io/v1", "VirtualMachineInstance", ns, "seed", status(map[string]any{"nodeName": node})),
		obj("kubevirt.io/v1", "VirtualMachine", ns, "target", status(map[string]any{"printableStatus": "Stopped"})),
		vsno("b"), // a vSNO: the caller reports it with guest.List, not as a VM
	}
	for i, d := range cluster.Deployments {
		objs = append(objs, deployment(d, 1), service(d, fmt.Sprintf("172.30.0.%d", i+1)))
	}
	return objs
}

var listKinds = map[schema.GroupVersionResource]string{
	cluster.GVRCSV:             "ClusterServiceVersionList",
	cluster.GVRSubscription:    "SubscriptionList",
	cluster.GVRInstallPlan:     "InstallPlanList",
	cluster.GVRPackageManifest: "PackageManifestList",
	precheck.GVRClusterVersion: "ClusterVersionList",
	precheck.GVRLVMCluster:     "LVMClusterList",
	cluster.GVRVirtualMachine:  "VirtualMachineList",
}

func kube(withState, managed bool) *kfake.Clientset {
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if managed {
		n.Labels = map[string]string{cluster.ManagedByLabel: "vsno"}
	}
	k := kfake.NewClientset(n,
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: manifests.DefaultStorageClass}},
		&storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: node}, Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: cluster.TopoLVM}}}},
	)
	if withState {
		st := &state.State{Node: node, Disk: "/dev/disk/by-id/wwn-0x5000000000000002"}
		for i := range 7 {
			st.Record("setup", state.Done, "", time.Date(2026, 11, 18, i, 0, 0, 0, time.UTC))
		}
		if err := state.Save(context.Background(), k, ns, st); err != nil {
			panic(err)
		}
	}
	return k
}

func byName(cs []Component) map[string]Component {
	m := map[string]Component{}
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

func TestCollectHealthy(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, healthy()...)
	r, err := Collect(context.Background(), kube(true, true), dyn, ns)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Exists || !r.Managed || r.Node != node || len(r.Stages) != 7 || !r.Healthy() {
		t.Fatalf("report = %+v", r)
	}
	for _, c := range append(r.Host, r.Support...) {
		if c.Status != precheck.Pass {
			t.Errorf("%s = %s %q", c.Name, c.Status, c.Detail)
		}
	}
	if d := byName(r.Host)["storage"].Detail; !strings.Contains(d, "on "+node) {
		t.Errorf("storage detail = %q", d)
	}
	if r.Registry != "172.30.0.4:5000" {
		t.Errorf("registry = %q", r.Registry)
	}
	if d := byName(r.Support)["dnsmasq"].Detail; d != "1/1 ready, 172.30.0.1" {
		t.Errorf("dnsmasq = %q", d)
	}
	want := []VM{{Name: "seed", Status: "Running", Ready: true, Node: node}, {Name: "target", Status: "Stopped"}}
	if len(r.VMs) != 2 || r.VMs[0] != want[0] || r.VMs[1] != want[1] {
		t.Errorf("vms = %+v", r.VMs)
	}
}

func TestCollectDegraded(t *testing.T) {
	objs := []runtime.Object{
		obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", cluster.CNVNamespace, "kubevirt-hyperconverged-operator.v1", status(map[string]any{"phase": "Installing"})),
		obj("hco.kubevirt.io/v1beta1", "HyperConverged", cluster.CNVNamespace, cluster.HyperConvergedName, available("False")),
		obj("lvm.topolvm.io/v1alpha1", "LVMCluster", cluster.StorageNamespace, cluster.LVMClusterName, status(map[string]any{"state": "Progressing"})),
		deployment("dnsmasq", 0),
	}
	k := kube(true, false) // unlabeled, but has state: still managed
	_ = k.StorageV1().CSINodes().Delete(context.Background(), node, metav1.DeleteOptions{})
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	r, err := Collect(context.Background(), k, dyn, ns)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Managed || r.Healthy() {
		t.Errorf("managed=%v healthy=%v", r.Managed, r.Healthy())
	}
	host, sup := byName(r.Host), byName(r.Support)
	for name, want := range map[string]precheck.Status{"cnv": precheck.Warn, "lvms": precheck.Fail, "hyperconverged": precheck.Warn,
		"lvmcluster": precheck.Warn, "storage": precheck.Warn} {
		if host[name].Status != want {
			t.Errorf("%s = %s (%q), want %s", name, host[name].Status, host[name].Detail, want)
		}
	}
	if sup["dnsmasq"].Status != precheck.Warn || sup["registry"].Status != precheck.Fail || r.Registry != "" {
		t.Errorf("support = %+v registry %q", r.Support, r.Registry)
	}
	if !strings.Contains(host["lvms"].Detail, "vsno setup") {
		t.Errorf("missing operator hint: %q", host["lvms"].Detail)
	}
}

func TestCollectMissingNamespace(t *testing.T) {
	k := kfake.NewClientset()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	r, err := Collect(context.Background(), k, dyn, ns)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exists || r.Managed || r.Healthy() || r.Support != nil || len(r.Host) != 6 {
		t.Errorf("report = %+v", r)
	}
	if c := byName(r.Host)["storage"]; c.Status != precheck.Fail {
		t.Errorf("storage = %+v", c)
	}
	if c := byName(r.Host)["lvmcluster"]; c.Status != precheck.Fail {
		t.Errorf("lvmcluster = %+v", c)
	}
}

func TestCollectUnmanagedNamespace(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, healthy()...)
	r, err := Collect(context.Background(), kube(false, false), dyn, ns)
	if err != nil || r.Managed || r.Node != "" {
		t.Fatalf("report = %+v %v", r, err)
	}
	if d := byName(r.Host)["storage"].Detail; d != "StorageClass "+manifests.DefaultStorageClass {
		t.Errorf("storage without a known node = %q", d)
	}
}

func TestCollectReportsOperatorUpdate(t *testing.T) {
	const wantCSV = "kubevirt-hyperconverged-operator.v2"
	objs := append(healthy(),
		obj("packages.operators.coreos.com/v1", "PackageManifest", cluster.MarketplaceNamespace, cluster.CNV.Package,
			map[string]any{"status": map[string]any{"defaultChannel": "stable",
				"channels": []any{map[string]any{"name": "stable", "currentCSV": wantCSV}}}}),
		obj("operators.coreos.com/v1alpha1", "Subscription", cluster.CNVNamespace, "hco-operatorhub",
			map[string]any{"spec": map[string]any{"name": cluster.CNV.Package, "channel": "stable"},
				"status": map[string]any{
					"installedCSV": "kubevirt-hyperconverged-operator.v1",
					"currentCSV":   "kubevirt-hyperconverged-operator.v1",
					"state":        "UpgradeAvailable",
				}}),
	)
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	r, err := Collect(context.Background(), kube(true, true), dyn, ns)
	if err != nil {
		t.Fatal(err)
	}
	cnv := byName(r.Host)["cnv"]
	if cnv.Status != precheck.Warn || !strings.Contains(cnv.Detail, "update available (") || !strings.Contains(cnv.Detail, wantCSV) {
		t.Errorf("cnv = %+v", cnv)
	}
	// A warning keeps status exiting 0.
	if !r.Healthy() {
		t.Error("an operator update must not make the environment unhealthy")
	}
	if lvms := byName(r.Host)["lvms"]; lvms.Status != precheck.Pass {
		t.Errorf("lvms = %+v", lvms)
	}
}

func TestCollectWithoutKubeVirt(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, healthy()...)
	dyn.PrependReactor("list", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"}, "")
	})
	r, err := Collect(context.Background(), kube(true, true), dyn, ns)
	if err != nil || r.VMs != nil || !r.Healthy() {
		t.Errorf("no CRD: vms=%v healthy=%v err=%v", r.VMs, r.Healthy(), err)
	}
}

func fails(verb, resource string) k8stesting.ReactionFunc {
	return func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			return true, nil, errors.New(resource + " down")
		}
		return false, nil, nil
	}
}

func TestCollectErrors(t *testing.T) {
	ctx := context.Background()
	newDyn := func() *dynfake.FakeDynamicClient {
		return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, healthy()...)
	}

	k := kube(true, true)
	k.PrependReactor("get", "namespaces", fails("get", "namespaces"))
	if _, err := Collect(ctx, k, newDyn(), ns); err == nil || !strings.Contains(err.Error(), "namespaces down") {
		t.Errorf("namespace error = %v", err)
	}

	k = kube(false, true)
	k.PrependReactor("get", "configmaps", fails("get", "configmaps"))
	if _, err := Collect(ctx, k, newDyn(), ns); err == nil {
		t.Error("want the state read error")
	}

	// Component errors become rows, not a failed Collect.
	k = kube(true, true)
	k.PrependReactor("get", "storageclasses", fails("get", "storageclasses"))
	dyn := newDyn()
	for _, r := range []struct{ verb, res string }{
		{"list", "clusterserviceversions"}, {"get", "hyperconvergeds"}, {"list", "lvmclusters"},
		{"get", "deployments"}, {"list", "virtualmachines"},
	} {
		dyn.PrependReactor(r.verb, r.res, fails(r.verb, r.res))
	}
	rep, err := Collect(ctx, k, dyn, ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(rep.Host, rep.Support...) {
		want := precheck.Fail
		if c.Name == "vms" || c.Name == "density" { // informational: they only warn
			want = precheck.Warn
		}
		if c.Status != want || !strings.Contains(c.Detail, "down") {
			t.Errorf("%s = %s %q", c.Name, c.Status, c.Detail)
		}
	}
}

func vsno(name string) *unstructured.Unstructured {
	u := obj("kubevirt.io/v1", "VirtualMachine", ns, name, status(map[string]any{"printableStatus": "Running"}))
	u.SetLabels(map[string]string{guest.LabelName: name})
	return u
}
