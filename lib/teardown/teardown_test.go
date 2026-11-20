package teardown

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
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
		cluster.GVRCSV:            "ClusterServiceVersionList",
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
	if c.answer != ns || c.shown != 1 || !strings.Contains(c.asked, "This deletes 21 object(s)") {
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
	if err != nil || res.Targets[0].Outcome != Deleted || len(res.Blockers) != 1 || !res.Forced {
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
	// Every step is done, and its objects with it.
	for _, s := range res.Steps {
		if s.Outcome != Deleted || !strings.HasSuffix(s.Detail, " deleted") {
			t.Errorf("step %s = %s %q", s.Name, s.Outcome, s.Detail)
		}
	}
	if n := apply.Count(res.Changes(), apply.Removed); n != len(res.Objects) {
		t.Errorf("%d of %d objects marked deleted", n, len(res.Objects))
	}
	// A CSV isn't among setup's objects but is in its operator's step.
	if !slices.ContainsFunc(res.Objects, func(o Object) bool { return o.Kind == "ClusterServiceVersion" && o.Step == "cnv" }) {
		t.Error("the CNV CSV is missing from the cnv step")
	}
}

// fakePlanner reports the refs in absent as missing and everything else as vsno's.
type fakePlanner struct {
	absent map[string]bool
	err    error
}

func (f fakePlanner) Plan(_ context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	ch := make([]apply.Change, 0, len(objs))
	for _, o := range objs {
		c := apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: apply.Reapply}
		if f.absent[c.Ref()] {
			c.Action = apply.Create
		}
		ch = append(ch, c)
	}
	return ch, f.err
}

func TestRunStepsMirrorSetup(t *testing.T) {
	e := (&confirmer{}).env(newKube(lvmsPVC(ns, "seed-root")), newDyn(host()...))
	e.Planner = fakePlanner{absent: map[string]bool{ns + "/Secret/registry-ca": true}}
	res, err := Run(context.Background(), e, Options{Namespace: ns, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range manifests.Steps {
		names = append(names, s.Name)
	}
	slices.Reverse(names)
	var got []string
	for _, s := range res.Steps {
		got = append(got, s.Name)
		if s.Name == "cnv" && (s.Outcome != Kept || s.Detail != "pass --host to remove") {
			t.Errorf("without --host, cnv = %+v", s)
		}
	}
	if !slices.Equal(got, names) {
		t.Errorf("steps = %v, want setup's reversed: %v", got, names)
	}
	if s := res.Steps[0]; s.Name != "support" || s.Outcome != Planned || s.Detail != "17 to delete, 1 absent" {
		t.Errorf("support = %+v", s)
	}
	if first, last := res.Objects[0], res.Objects[len(res.Objects)-1]; first.Ref() != ns+"/Deployment/registry" || last.Ref() != "Namespace/"+ns {
		t.Errorf("objects run %s .. %s, want setup's order reversed", first.Ref(), last.Ref())
	}
	// setup's builder-work PVC is expected; the VM and the other PVC are not.
	if want := []string{"VM seed", "PVC seed-root"}; !slices.Equal(res.Unmanaged, want) {
		t.Errorf("unmanaged = %v, want %v", res.Unmanaged, want)
	}
}

// With the namespace already gone, its steps have nothing to delete, while
// --host still has work.
func TestRunStepWithNothingToDeleteIsGone(t *testing.T) {
	absent := map[string]bool{}
	for _, st := range manifests.Steps {
		objs, err := st.Objects(manifests.NamesConfig("gone-env"))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			if !st.Host {
				absent[apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}.Ref()] = true
			}
		}
	}
	e := (&confirmer{}).env(newKube(), newDyn(host()...))
	e.Planner = fakePlanner{absent: absent}
	res, err := Run(context.Background(), e, Options{Namespace: "gone-env", Host: true, DryRun: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"support": "gone 18 absent", "namespace": "gone 3 absent", "storage": "planned 1 to delete"}
	for _, s := range res.Steps {
		if w, ok := want[s.Name]; ok && string(s.Outcome)+" "+s.Detail != w {
			t.Errorf("%s = %s %q, want %s", s.Name, s.Outcome, s.Detail, w)
		}
	}
}

func TestRunNamesVSNOsOnce(t *testing.T) {
	b := vm(ns, "b")
	b.SetLabels(map[string]string{guest.LabelName: "b"})
	objs := []runtime.Object{namespace(ns, true), b} // no plain VM, unlike host()
	e := (&confirmer{}).env(newKube(lvmsPVC(ns, "b-root"), lvmsPVC(ns, "b-iso"), lvmsPVC(ns, "data")), newDyn(objs...))
	res, err := Run(context.Background(), e, Options{Namespace: ns, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	// vSNO b's disks go with it; only the PVC nobody owns is listed separately.
	if want := []string{"vSNO b", "PVC data"}; !slices.Equal(res.Unmanaged, want) {
		t.Errorf("unmanaged = %v, want %v", res.Unmanaged, want)
	}
}

func TestRunPlannerError(t *testing.T) {
	e := (&confirmer{}).env(newKube(), newDyn(host()...))
	e.Planner = fakePlanner{err: errors.New("api down")}
	if _, err := Run(context.Background(), e, Options{Namespace: ns, DryRun: true}); err == nil || !strings.Contains(err.Error(), "api down") {
		t.Errorf("planner error = %v", err)
	}
}

func TestRunMarksOnlyFinishedSteps(t *testing.T) {
	d := newDyn(host()...)
	d.PrependReactor("delete", "lvmclusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("denied")
	})
	res, err := Run(context.Background(), (&confirmer{}).env(newKube(), d), Options{Namespace: ns, Host: true, Yes: true})
	if err == nil {
		t.Fatal("want the LVMCluster deletion to fail")
	}
	want := map[string]Outcome{"support": Deleted, "namespace": Deleted, "hyperconverged": Deleted, "cnv": Deleted, "storage": Planned, "lvms": Planned}
	for _, s := range res.Steps {
		if s.Outcome != want[s.Name] {
			t.Errorf("%s = %s, want %s", s.Name, s.Outcome, want[s.Name])
		}
	}
}

func TestPlanDetail(t *testing.T) {
	c := func(a apply.Action) apply.Change { return apply.Change{Action: a} }
	for _, tc := range []struct {
		ch   []apply.Change
		want string
	}{
		{nil, "nothing to delete"},
		{[]apply.Change{c(apply.Delete)}, "1 to delete"},
		{[]apply.Change{c(apply.Absent), c(apply.Delete), c(apply.Absent)}, "1 to delete, 2 absent"},
	} {
		if got := PlanDetail(tc.ch); got != tc.want {
			t.Errorf("PlanDetail = %q, want %q", got, tc.want)
		}
	}
}

func TestRunHostBlockers(t *testing.T) {
	objs := append(host(), vm("other", "web"), obj("lvm.topolvm.io/v1alpha1", "LVMCluster", cluster.StorageNamespace, "theirs"))
	k := newKube(lvmsPVC("other", "data"), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "no-class"}})
	d := newDyn(objs...)
	c := &confirmer{ok: true}
	res, err := Run(context.Background(), c.env(k, d), Options{Namespace: ns, Host: true})
	if err == nil || !strings.Contains(err.Error(), "3 problem(s)") || !strings.Contains(err.Error(), "other/web; 1 PVC(s)") || !strings.Contains(err.Error(), "; LVMCluster openshift-storage/theirs") {
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
	if c.asked != "" || !exists(t, d, cluster.GVRNamespace, "", ns) || res.Forced {
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
		{"list", "lvmclusters"}, {"get", "lvmclusters"}, {"get", "hyperconvergeds"},
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
