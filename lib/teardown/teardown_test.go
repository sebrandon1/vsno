package teardown

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
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

const ns = "vsno-test"

func obj(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func namespace(name string, managed bool) *unstructured.Unstructured {
	u := obj("v1", "Namespace", "", name)
	if managed {
		u.SetLabels(map[string]string{cluster.ManagedByLabel: "vsno"})
	}
	return u
}

func vm(namespace, name string) *unstructured.Unstructured {
	return obj("kubevirt.io/v1", "VirtualMachine", namespace, name)
}

func sub(namespace, name, pkg string) *unstructured.Unstructured {
	u := obj("operators.coreos.com/v1alpha1", "Subscription", namespace, name)
	u.Object["spec"] = map[string]any{"name": pkg}
	return u
}

// host returns the objects setup leaves on a cluster.
func host() []runtime.Object {
	return []runtime.Object{
		namespace(ns, true),
		vm(ns, "seed"),
		namespace(cluster.CNVNamespace, false),
		namespace(cluster.StorageNamespace, false),
		namespace(cluster.OSImagesNamespace, false),
		obj("hco.kubevirt.io/v1beta1", "HyperConverged", cluster.CNVNamespace, cluster.HyperConvergedName),
		sub(cluster.CNVNamespace, "hco-operatorhub", "kubevirt-hyperconverged"),
		sub(cluster.CNVNamespace, "unrelated", "something-else"),
		obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", cluster.CNVNamespace, "kubevirt-hyperconverged-operator.v1"),
		obj("lvm.topolvm.io/v1alpha1", "LVMCluster", cluster.StorageNamespace, cluster.LVMClusterName),
		sub(cluster.StorageNamespace, "lvms", "lvms-operator"),
		obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", cluster.StorageNamespace, "lvms-operator.v1"),
	}
}

func newDyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		precheck.GVRCSV:           "ClusterServiceVersionList",
		precheck.GVRLVMCluster:    "LVMClusterList",
		cluster.GVRVirtualMachine: "VirtualMachineList",
		cluster.GVRSubscription:   "SubscriptionList",
	}, objs...)
}

func lvmsPVC(namespace, name string) *corev1.PersistentVolumeClaim {
	sc := manifests.DefaultStorageClass
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc}}
}

func newKube(objs ...runtime.Object) *kfake.Clientset {
	objs = append(objs,
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: manifests.DefaultStorageClass}, Provisioner: cluster.TopoLVM},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Provisioner: "example.com/csi"},
		lvmsPVC(ns, "builder-work"),
		lvmsPVC(cluster.OSImagesNamespace, "rhel9"),
	)
	return kfake.NewClientset(objs...)
}

// confirmer records the question and answers yes when ok.
type confirmer struct {
	ok            bool
	asked, answer string
	shown         int
}

func (c *confirmer) env(k *kfake.Clientset, d *dynfake.FakeDynamicClient) Env {
	return Env{Kube: k, Dyn: d, Poll: time.Millisecond, Timeout: 50 * time.Millisecond,
		Confirm:  func(q, a string) bool { c.asked, c.answer = q, a; return c.ok },
		ShowPlan: func(*Result) { c.shown++ },
	}
}

func refs(res *Result) []string {
	out := make([]string, 0, len(res.Targets))
	for _, t := range res.Targets {
		out = append(out, t.Ref())
	}
	return out
}

func exists(t *testing.T, d *dynfake.FakeDynamicClient, gvr schema.GroupVersionResource, namespace, name string) bool {
	t.Helper()
	_, err := d.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestRunNamespace(t *testing.T) {
	d := newDyn(host()...)
	c := &confirmer{ok: true}
	res, err := Run(context.Background(), c.env(newKube(), d), Options{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	if got := refs(res); !slices.Equal(got, []string{"Namespace " + ns}) {
		t.Fatalf("targets = %v", got)
	}
	if tg := res.Targets[0]; tg.Outcome != Deleted || tg.Detail != "1 VM(s) (seed), 1 PVC(s)" {
		t.Errorf("target = %+v", tg)
	}
	if c.answer != ns || c.shown != 1 || !strings.Contains(c.asked, "1 object(s)") {
		t.Errorf("confirm: answer %q shown %d q %q", c.answer, c.shown, c.asked)
	}
	if exists(t, d, cluster.GVRNamespace, "", ns) || !exists(t, d, precheck.GVRHyperConverged, cluster.CNVNamespace, cluster.HyperConvergedName) {
		t.Error("should delete the namespace only")
	}

	// A second run has nothing to do and doesn't ask.
	c = &confirmer{}
	res, err = Run(context.Background(), c.env(newKube(), d), Options{Namespace: ns})
	if err != nil || len(res.Targets) != 0 || c.asked != "" {
		t.Errorf("rerun: %v %v asked %q", res.Targets, err, c.asked)
	}
}

func TestRunNamespaceOwnership(t *testing.T) {
	ctx := context.Background()
	d := newDyn(namespace(ns, false))
	c := &confirmer{ok: true}
	res, err := Run(ctx, c.env(newKube(), d), Options{Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "not created by vsno") || len(res.Blockers) != 1 || c.asked != "" {
		t.Fatalf("unowned namespace: %v %+v", err, res)
	}
	if !exists(t, d, cluster.GVRNamespace, "", ns) {
		t.Error("deleted an unowned namespace")
	}

	// A state ConfigMap shows vsno set it up.
	k := newKube()
	if err := state.Save(ctx, k, ns, &state.State{Node: "worker-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, c.env(k, d), Options{Namespace: ns, DryRun: true}); err != nil {
		t.Errorf("namespace with state: %v", err)
	}

	// An unreadable state is an error, not a guess.
	k = newKube()
	k.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("api down") })
	if _, err := Run(ctx, c.env(k, d), Options{Namespace: ns, DryRun: true}); err == nil || !strings.Contains(err.Error(), "api down") {
		t.Errorf("state read error = %v", err)
	}

	// --force overrides.
	res, err = Run(ctx, c.env(newKube(), d), Options{Namespace: ns, Force: true, Yes: true})
	if err != nil || res.Targets[0].Outcome != Deleted || len(res.Blockers) != 1 {
		t.Errorf("forced: %v %+v", err, res)
	}
}

func TestRunNotConfirmed(t *testing.T) {
	d := newDyn(host()...)
	for _, e := range []Env{(&confirmer{}).env(newKube(), d), {Kube: newKube(), Dyn: d}} { // refused, and no Confirm at all
		res, err := Run(context.Background(), e, Options{Namespace: ns})
		if err == nil || !strings.Contains(err.Error(), "not confirmed") || res.Targets[0].Outcome != Planned {
			t.Errorf("got %v %+v", err, res.Targets)
		}
	}
	if !exists(t, d, cluster.GVRNamespace, "", ns) {
		t.Error("deleted without confirmation")
	}
}

func TestRunHostDryRun(t *testing.T) {
	d := newDyn(host()...)
	c := &confirmer{}
	res, err := Run(context.Background(), c.env(newKube(), d), Options{Namespace: ns, Host: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Namespace " + ns,
		"HyperConverged openshift-cnv/kubevirt-hyperconverged",
		"Namespace openshift-virtualization-os-images",
		"Subscription openshift-cnv/hco-operatorhub",
		"ClusterServiceVersion openshift-cnv/kubevirt-hyperconverged-operator.v1",
		"Namespace openshift-cnv",
		"LVMCluster openshift-storage/vsno-lvmcluster",
		"Subscription openshift-storage/lvms",
		"ClusterServiceVersion openshift-storage/lvms-operator.v1",
		"Namespace openshift-storage",
	}
	if got := refs(res); !slices.Equal(got, want) {
		t.Fatalf("targets:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, tg := range res.Targets {
		if tg.Outcome != Planned {
			t.Errorf("%s = %s", tg.Ref(), tg.Outcome)
		}
	}
	if c.asked != "" || !exists(t, d, precheck.GVRLVMCluster, cluster.StorageNamespace, cluster.LVMClusterName) {
		t.Error("dry run asked or deleted")
	}
}

func TestRunHostDeletes(t *testing.T) {
	d := newDyn(host()...)
	c := &confirmer{ok: true}
	res, err := Run(context.Background(), c.env(newKube(), d), Options{Namespace: ns, Host: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.answer != "host" {
		t.Errorf("answer = %q", c.answer)
	}
	for _, tg := range res.Targets {
		if tg.Outcome != Deleted || exists(t, d, tg.gvr, tg.Namespace, tg.Name) {
			t.Errorf("%s = %s", tg.Ref(), tg.Outcome)
		}
	}
	if !exists(t, d, cluster.GVRSubscription, cluster.CNVNamespace, "unrelated") {
		t.Error("deleted a Subscription for another package")
	}
}

func TestRunHostBlockers(t *testing.T) {
	objs := append(host(), vm("other", "web"), obj("lvm.topolvm.io/v1alpha1", "LVMCluster", cluster.StorageNamespace, "theirs"))
	k := newKube(lvmsPVC("other", "data"), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "no-class"}})
	d := newDyn(objs...)
	c := &confirmer{ok: true}
	res, err := Run(context.Background(), c.env(k, d), Options{Namespace: ns, Host: true})
	if err == nil || !strings.Contains(err.Error(), "3 problem(s)") || !strings.Contains(err.Error(), "and 2 more") {
		t.Fatalf("err = %v", err)
	}
	want := []string{
		"VMs outside vsno-test still exist: other/web",
		"1 PVC(s) outside vsno-test use LVMS storage: other/data",
		"LVMCluster openshift-storage/theirs was not created by vsno",
	}
	if !slices.Equal(res.Blockers, want) {
		t.Errorf("blockers = %q", res.Blockers)
	}
	if c.asked != "" || !exists(t, d, cluster.GVRNamespace, "", ns) {
		t.Error("blocked run asked or deleted")
	}
}

func TestRunDeleteErrors(t *testing.T) {
	ctx := context.Background()

	// The namespace disappears between planning and deleting.
	d := newDyn(host()...)
	d.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
	})
	res, err := Run(ctx, (&confirmer{}).env(newKube(), d), Options{Namespace: ns, Yes: true})
	if err != nil || res.Targets[0].Outcome != Gone {
		t.Errorf("gone: %v %+v", err, res.Targets)
	}

	d = newDyn(host()...)
	d.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := Run(ctx, (&confirmer{}).env(newKube(), d), Options{Namespace: ns, Yes: true}); err == nil || !strings.Contains(err.Error(), "deleting Namespace "+ns) {
		t.Errorf("delete error = %v", err)
	}

	// The namespace never goes away (a stuck finalizer).
	d = newDyn(host()...)
	d.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	res, err = Run(ctx, (&confirmer{}).env(newKube(), d), Options{Namespace: ns, Yes: true})
	if err == nil || !strings.Contains(err.Error(), "waiting for Namespace "+ns+" to be deleted") || res.Targets[0].Outcome != Deleted {
		t.Errorf("stuck: %v %+v", err, res.Targets)
	}
}

func TestRunPlanErrors(t *testing.T) {
	for _, r := range []struct{ verb, res string }{
		{"get", "namespaces"}, {"list", "virtualmachines"}, {"list", "persistentvolumeclaims"},
		{"list", "storageclasses"}, {"list", "subscriptions"}, {"list", "clusterserviceversions"},
		{"list", "lvmclusters"}, {"get", "hyperconvergeds"},
	} {
		// Without the environment namespace, the host checks make the first call.
		for _, envNS := range []string{ns, "absent"} {
			t.Run(r.verb+" "+r.res+" in "+envNS, func(t *testing.T) {
				k, d := newKube(), newDyn(host()...)
				fail := func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("api down") }
				k.PrependReactor(r.verb, r.res, fail)
				d.PrependReactor(r.verb, r.res, fail)
				if _, err := Run(context.Background(), (&confirmer{}).env(k, d), Options{Namespace: envNS, Host: true, DryRun: true}); err == nil || !strings.Contains(err.Error(), "api down") {
					t.Errorf("err = %v", err)
				}
			})
		}
	}
}

func TestRunNoCRDs(t *testing.T) {
	// A cluster without KubeVirt or LVMS: the lists return NotFound.
	d := newDyn(namespace(ns, true))
	notFound := func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: a.GetResource().Resource}, "")
	}
	for _, r := range []string{"virtualmachines", "lvmclusters", "subscriptions", "clusterserviceversions"} {
		d.PrependReactor("list", r, notFound)
	}
	res, err := Run(context.Background(), (&confirmer{}).env(kfake.NewClientset(), d), Options{Namespace: ns, Host: true, DryRun: true})
	if err != nil || !slices.Equal(refs(res), []string{"Namespace " + ns}) {
		t.Errorf("got %v %v", refs(res), err)
	}
}

func TestShort(t *testing.T) {
	if got := short([]string{"a", "b"}); got != "a, b" {
		t.Errorf("short = %q", got)
	}
	if got := short([]string{"1", "2", "3", "4", "5", "6", "7"}); got != "1, 2, 3, 4, 5, and 2 more" {
		t.Errorf("short = %q", got)
	}
}
