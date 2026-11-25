package precheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fixtureProber map[string]string // node -> fixture file

func (f fixtureProber) Probe(_ context.Context, nodes []string, onDone func(string, error)) (map[string]NodeHardware, map[string]error) {
	hw, errs := map[string]NodeHardware{}, map[string]error{}
	for _, n := range nodes {
		b, err := os.ReadFile(filepath.Join("testdata", f[n]))
		if err != nil {
			errs[n] = err
			continue
		}
		h, err := ParseProbe(string(b))
		if onDone != nil {
			onDone(n, err)
		}
		if err != nil {
			errs[n] = err
			continue
		}
		hw[n] = h
	}
	return hw, errs
}

func node(name string, role string, memGi int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node-role.kubernetes.io/" + role: ""}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(memGi<<30, resource.BinarySI)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func obj(gvr schema.GroupVersionResource, kind, ns, name string, fields map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	u.SetAPIVersion(gvr.GroupVersion().String())
	u.SetKind(kind)
	u.SetName(name)
	u.SetNamespace(ns)
	return u
}

func packageManifest(name, def string, channels ...string) *unstructured.Unstructured {
	var chs []interface{}
	for _, c := range channels {
		chs = append(chs, map[string]interface{}{"name": c})
	}
	return obj(cluster.GVRPackageManifest, "PackageManifest", cluster.MarketplaceNamespace, name,
		map[string]interface{}{"status": map[string]interface{}{"defaultChannel": def, "channels": chs}})
}

// packageManifestCSV is packageManifest with each channel's currentCSV set.
func packageManifestCSV(name, def string, csvs map[string]string, channels ...string) *unstructured.Unstructured {
	var chs []interface{}
	for _, c := range channels {
		chs = append(chs, map[string]interface{}{"name": c, "currentCSV": csvs[c]})
	}
	return obj(cluster.GVRPackageManifest, "PackageManifest", cluster.MarketplaceNamespace, name,
		map[string]interface{}{"status": map[string]interface{}{"defaultChannel": def, "channels": chs}})
}

func newDyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{
		GVRClusterVersion: "ClusterVersionList", GVRNetwork: "NetworkList",
		cluster.GVRPackageManifest: "PackageManifestList", cluster.GVRCSV: "ClusterServiceVersionList",
		cluster.GVRSubscription: "SubscriptionList", cluster.GVRInstallPlan: "InstallPlanList",
		cluster.GVRHyperConverged: "HyperConvergedList", GVRLVMCluster: "LVMClusterList",
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
}

func clusterAdmin(k *kfake.Clientset, allowed bool) {
	k.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{Status: authv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
}

// labEnv: 3 masters, one VM worker (4 cores, nested KVM), one bare-metal bare-metal worker.
func labEnv(t *testing.T, bmFixture string, extraKube ...runtime.Object) *Env {
	t.Helper()
	kubeObjs := append([]runtime.Object{
		node("master-0", "master", 32), node("master-1", "master", 32), node("master-2", "master", 32),
		node("vm-worker", "worker", 16), node("bm-worker", "worker", 93),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "dns-default", Namespace: "openshift-dns"},
			Spec: corev1.ServiceSpec{ClusterIP: "172.30.0.10"}},
	}, extraKube...)
	k := kfake.NewSimpleClientset(kubeObjs...)
	clusterAdmin(k, true)

	dyn := newDyn(
		obj(GVRClusterVersion, "ClusterVersion", "", "version",
			map[string]interface{}{"status": map[string]interface{}{"desired": map[string]interface{}{"version": "4.22.0-0.nightly-2026-11-03-073516"}}}),
		obj(GVRNetwork, "Network", "", "cluster", map[string]interface{}{"spec": map[string]interface{}{
			"clusterNetwork": []interface{}{map[string]interface{}{"cidr": "10.128.0.0/14", "hostPrefix": int64(23)}},
			"serviceNetwork": []interface{}{"172.30.0.0/16"},
		}}),
		packageManifest("kubevirt-hyperconverged", "stable", "candidate", "stable"),
		packageManifest("lvms-operator", "stable-4.22", "stable-4.21", "stable-4.22"),
	)
	return &Env{
		Kube: k, Dyn: dyn, Namespace: "vsno",
		Prober: fixtureProber{"vm-worker": "probe-vm-worker.txt", "bm-worker": bmFixture},
	}
}

func find(r *Report, check string) *Result {
	for i := range r.Results {
		if r.Results[i].Name() == check {
			return &r.Results[i]
		}
	}
	return nil
}

func TestRunPicksBareMetalNodeAndDisk(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	r := Run(context.Background(), env, DefaultRequirements(), Options{})

	if r.Failed() != 0 {
		for _, res := range r.Results {
			t.Logf("%-5s %-40s %s", res.Status, res.Name(), res.Detail)
		}
		t.Fatalf("an eligible node exists, so nothing should fail; got %d failures", r.Failed())
	}
	if got := find(r, "Eligible node"); got == nil || got.Status != Pass {
		t.Fatalf("want an eligible node, got %+v", got)
	}
	want := Suggested{
		Node: "bm-worker", Disk: "/dev/disk/by-id/wwn-0x5000000000000002", DiskSizeGB: 1919,
		HostVersion: "4.22.0-0.nightly-2026-11-03-073516", HostDNS: "172.30.0.10",
		LVMSChannel: "stable-4.22", CNVChannel: "stable",
	}
	if r.Suggested != want {
		t.Fatalf("Suggested =\n %+v\nwant\n %+v", r.Suggested, want)
	}
	// The VM worker is rejected (too few CPUs, too little memory, disk too small),
	// reported as info because the bare-metal will be used instead.
	for _, c := range []string{"vm-worker: cpu", "vm-worker: memory", "vm-worker: disk"} {
		if got := find(r, c); got == nil || got.Status != Info || !strings.HasPrefix(got.Detail, "not usable: ") {
			t.Errorf("%s: want info 'not usable', got %+v", c, got)
		}
	}
	if got := find(r, "vm-worker: virtualization"); got == nil || got.Status != Warn {
		t.Errorf("nested-KVM VM worker should warn, got %+v", got)
	}
	if got := find(r, "Network overlap"); got == nil || got.Status != Pass {
		t.Errorf("network overlap: %+v", got)
	}
}

func TestRunGroupsAndNodes(t *testing.T) {
	r := Run(context.Background(), labEnv(t, "probe-baremetal-empty-disk.txt"), DefaultRequirements(), Options{})
	wantNodes := []NodeSummary{{Name: "bm-worker", Eligible: true}, {Name: "vm-worker"}}
	if !slices.Equal(r.Nodes, wantNodes) {
		t.Errorf("Nodes = %+v, want %+v", r.Nodes, wantNodes)
	}
	// Each row has its section; per-node rows carry the node.
	for name, want := range map[string]Group{
		"Cluster access": GroupCluster, "OpenShift version": GroupCluster,
		"Catalog: LVM Storage": GroupInstall, "Seed registry": GroupInstall,
		"Worker nodes": GroupWorkers, "bm-worker: cpu": GroupWorkers, "Eligible node": GroupWorkers,
	} {
		if got := find(r, name); got == nil || got.Group != want {
			t.Errorf("%s: group = %+v, want %s", name, got, want)
		}
	}
	if got := find(r, "bm-worker: cpu"); got.Node != "bm-worker" || got.Check != "cpu" {
		t.Errorf("per-node row = %+v", got)
	}
	// The per-node blocks name the workers, so the candidates row only counts them.
	if got := find(r, "Worker nodes"); got.Detail != "2 candidate(s)" {
		t.Errorf("worker nodes detail = %q", got.Detail)
	}
}

func TestRunProbeFailureIsANodeRow(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	env.Prober = fixtureProber{"bm-worker": "probe-baremetal-empty-disk.txt", "vm-worker": "missing.txt"}
	r := Run(context.Background(), env, DefaultRequirements(), Options{})
	got := find(r, "vm-worker: probe")
	if got == nil || got.Status != Warn || got.Node != "vm-worker" || got.Group != GroupWorkers {
		t.Fatalf("probe failure row = %+v", got)
	}
	// A node that wasn't evaluated has no verdict.
	if want := []NodeSummary{{Name: "bm-worker", Eligible: true}}; !slices.Equal(r.Nodes, want) {
		t.Errorf("Nodes = %+v", r.Nodes)
	}
}

func TestRunExistingLVMSDiskIsFineWhenStorageClassExists(t *testing.T) {
	env := labEnv(t, "probe-baremetal-lvms-disk.txt",
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: vsnoSC}},
		topoLVMNode("bm-worker"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vsno"}})
	r := Run(context.Background(), env, DefaultRequirements(), Options{})

	if got := find(r, "bm-worker: disk"); got == nil || got.Status != Info {
		t.Fatalf("existing LVMS storage should be info, got %+v", got)
	}
	if got := find(r, "Eligible node"); got == nil || got.Status != Pass || r.Suggested.Node != "bm-worker" {
		t.Fatalf("bm-worker should still be eligible: %+v suggested=%+v", got, r.Suggested)
	}
	if r.Suggested.Disk != "" {
		t.Errorf("no new disk should be suggested, got %q", r.Suggested.Disk)
	}
	// LVMS only serves the bare-metal, so the shortcut must not apply to vm-worker.
	if got := find(r, "vm-worker: disk"); got == nil || !strings.Contains(got.Detail, "not usable: no empty disk") {
		t.Errorf("vm-worker has no TopoLVM; its disk must be unusable, got %+v", got)
	}
	if got := find(r, "Existing: vsno environment"); got == nil {
		t.Error("existing vsno namespace should be reported")
	}
}

func TestRunReportsOperatorUpdates(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	// CNV's channel offers a newer CSV than the one it runs; LVMS is current.
	const cnvCSV, wantCSV, lvmsCSV = "kubevirt-hyperconverged-operator.v1", "kubevirt-hyperconverged-operator.v2", "lvms-operator.v1"
	tr := env.Dyn.(*dynfake.FakeDynamicClient).Tracker()
	pm := packageManifestCSV(cluster.CNV.Package, "stable", map[string]string{"stable": wantCSV}, "candidate", "stable")
	if err := tr.Update(cluster.GVRPackageManifest, pm, cluster.MarketplaceNamespace); err != nil {
		t.Fatal(err)
	}
	// Each object gets its own map: obj() writes the name into it.
	csvObj := func(ns, name string) *unstructured.Unstructured {
		return obj(cluster.GVRCSV, "ClusterServiceVersion", ns, name,
			map[string]interface{}{"status": map[string]interface{}{"phase": "Succeeded"}})
	}
	objs := []struct {
		gvr schema.GroupVersionResource
		obj *unstructured.Unstructured
	}{
		{cluster.GVRSubscription, obj(cluster.GVRSubscription, "Subscription", cluster.CNVNamespace, "hco-operatorhub", map[string]interface{}{
			"spec":   map[string]interface{}{"name": cluster.CNV.Package, "channel": "stable"},
			"status": map[string]interface{}{"installedCSV": cnvCSV, "currentCSV": cnvCSV, "state": "UpgradeAvailable"},
		})},
		{cluster.GVRSubscription, obj(cluster.GVRSubscription, "Subscription", cluster.StorageNamespace, "lvms", map[string]interface{}{
			"spec":   map[string]interface{}{"name": cluster.LVMS.Package, "channel": "stable-4.22"},
			"status": map[string]interface{}{"installedCSV": lvmsCSV, "currentCSV": lvmsCSV, "state": "AtLatestKnown"},
		})},
		{cluster.GVRCSV, csvObj(cluster.CNVNamespace, cnvCSV)},
		{cluster.GVRCSV, csvObj(cluster.StorageNamespace, lvmsCSV)},
	}
	for _, c := range objs {
		if err := tr.Create(c.gvr, c.obj, c.obj.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	r := Run(context.Background(), env, DefaultRequirements(), Options{})

	if r.Failed() != 0 {
		t.Errorf("an operator update only warns; got %d failure(s)", r.Failed())
	}
	cnv := find(r, "Existing: OpenShift Virtualization")
	if cnv == nil || cnv.Status != Warn || cnv.Detail != "update available ("+cnvCSV+" → "+wantCSV+")" {
		t.Errorf("cnv = %+v", cnv)
	}
	if cnv != nil && !strings.Contains(cnv.Hint, "vsno setup") {
		t.Errorf("cnv hint = %q", cnv.Hint)
	}
	if lvms := find(r, "Existing: LVM Storage operator"); lvms == nil || lvms.Status != Info || !strings.Contains(lvms.Detail, "up to date") {
		t.Errorf("lvms = %+v", lvms)
	}
}

func TestChannels(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	if cnv, lvms := Channels(context.Background(), env.Dyn); cnv != "stable" || lvms != "stable-4.22" {
		t.Errorf("channels = %q, %q", cnv, lvms)
	}
	// Without a catalog or a version, callers fall back to the Subscription's
	// own channel.
	if cnv, lvms := Channels(context.Background(), newDyn()); cnv != "" || lvms != "" {
		t.Errorf("channels = %q, %q", cnv, lvms)
	}
}

func TestRunNoEligibleNode(t *testing.T) {
	env := labEnv(t, "probe-baremetal-lvms-disk.txt") // disk taken, no StorageClass
	env.Prober = fixtureProber{"vm-worker": "probe-vm-worker.txt", "bm-worker": "probe-baremetal-lvms-disk.txt"}
	r := Run(context.Background(), env, DefaultRequirements(), Options{})
	got := find(r, "Eligible node")
	if got == nil || got.Status != Fail {
		t.Fatalf("want no eligible node, got %+v", got)
	}
	if d := find(r, "bm-worker: disk"); d == nil || !strings.Contains(d.Detail, "LVM members: sdb") {
		t.Errorf("bm-worker disk detail should mention the LVM member: %+v", d)
	}
}

func TestRunNotClusterAdminStopsEarly(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	k := kfake.NewSimpleClientset()
	clusterAdmin(k, false)
	env.Kube = k
	r := Run(context.Background(), env, DefaultRequirements(), Options{})
	if len(r.Results) != 1 || r.Results[0].Status != Fail {
		t.Fatalf("want a single failing access result, got %+v", r.Results)
	}
}

func TestRunNoProbeWarns(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	env.Prober = nil
	r := Run(context.Background(), env, DefaultRequirements(), Options{})
	if got := find(r, "Hardware probe"); got == nil || got.Status != Warn {
		t.Fatalf("want probe-skipped warning, got %+v", got)
	}
	if got := find(r, "Worker nodes"); got == nil || !strings.Contains(got.Detail, "2 candidate(s): ") {
		t.Errorf("without the probe the candidates are named: %+v", got)
	}
}

func TestRunNodeFilter(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	r := Run(context.Background(), env, DefaultRequirements(), Options{Node: "vm-worker"})
	if got := find(r, "Eligible node"); got == nil || got.Status != Fail {
		t.Fatalf("vm-worker alone is not eligible: %+v", got)
	}
	if find(r, "bm-worker: cpu") != nil {
		t.Fatal("bm-worker must not be probed when --node=vm-worker")
	}
}

func topoLVMNode(name string) *storagev1.CSINode {
	return &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: "topolvm.io", NodeID: name}}}}
}

// memPod is a running pod on the bm-worker node requesting gi GiB of memory.
func memPod(ns, name string, gi int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{NodeName: "bm-worker", Containers: []corev1.Container{{Name: "c",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(gi<<30, resource.BinarySI)}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// An existing vsno environment holds most of the bare-metal's memory. That memory is
// counted as available (vsno would reuse it); other workloads' memory is not.
func TestRunCountsExistingVsnoMemoryAsAvailable(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt",
		memPod("vsno", "virt-launcher-seed", 33),
		memPod("vsno", "virt-launcher-target", 33),
		memPod("other", "app", 10))
	r := Run(context.Background(), env, DefaultRequirements(), Options{})
	if got := find(r, "bm-worker: memory"); got == nil || got.Status != Pass {
		t.Fatalf("93-10=83 GiB available to vsno; want pass, got %+v", got)
	}
	held := false
	for _, res := range r.Results {
		if res.Name() == "bm-worker: memory" && res.Status == Info && strings.Contains(res.Detail, "66 GiB held by the existing vsno") {
			held = true
		}
	}
	if !held {
		t.Error("want an info row explaining memory held by the existing environment")
	}
	// The note follows the memory row it explains, not the disk row.
	var bm []string
	for _, res := range r.Results {
		if res.Node == "bm-worker" {
			bm = append(bm, res.Check)
		}
	}
	if want := []string{"virtualization", "cpu", "memory", "memory", "disk"}; !slices.Equal(bm, want) {
		t.Errorf("bm-worker rows = %v, want %v", bm, want)
	}

	// Without the vsno pods, the same 'other' usage plus a big foreign workload fails.
	env = labEnv(t, "probe-baremetal-empty-disk.txt", memPod("other", "big", 80))
	r = Run(context.Background(), env, DefaultRequirements(), Options{})
	if got := find(r, "bm-worker: memory"); got == nil || got.Status != Fail {
		t.Fatalf("13 GiB free must fail, got %+v", got)
	}
}

// recorder is a Progress that records events in order.
type recorder struct {
	mu     sync.Mutex
	events []string
}

type recStep struct {
	r     *recorder
	title string
}

func (r *recorder) log(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}
func (r *recorder) Start(t string) ProgressStep { r.log("start " + t); return recStep{r, t} }
func (s recStep) Update(string)                 {}
func (s recStep) SetProgress(d, t int)          { s.r.log(fmt.Sprintf("progress %s %d/%d", s.title, d, t)) }
func (s recStep) Done(m string)                 { s.r.log("done " + s.title + ": " + m) }
func (s recStep) Warn(m string)                 { s.r.log("warn " + s.title + ": " + m) }
func (s recStep) Fail(m string)                 { s.r.log("fail " + s.title + ": " + m) }

func TestRunReportsProgress(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	rec := &recorder{}
	env.Progress = rec
	Run(context.Background(), env, DefaultRequirements(), Options{})

	want := []string{
		"start Checking cluster configuration",
		"done Checking cluster configuration: 4.22.0-0.nightly-2026-11-03-073516",
		"start Probing 2 worker(s)",
		"progress Probing 2 worker(s) 0/2",
		"progress Probing 2 worker(s) 1/2",
		"progress Probing 2 worker(s) 2/2",
		"done Probing 2 worker(s): 2 probed",
	}
	if strings.Join(rec.events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(rec.events, "\n"), strings.Join(want, "\n"))
	}
}

func TestSeedRegistryDefaultsToInCluster(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	r := Run(context.Background(), env, DefaultRequirements(), Options{})
	got := find(r, "Seed registry")
	if got == nil || got.Status != Info || !strings.Contains(got.Detail, "in-cluster registry (default)") {
		t.Fatalf("no --seed-repo must mean the in-cluster registry, got %+v", got)
	}
}

// An overcommit or KSM already set on HyperConverged is reported, since
// vsno create counts capacity with it; with nothing set there is no row.
func TestRunReportsDensity(t *testing.T) {
	env := labEnv(t, "probe-baremetal-empty-disk.txt")
	if got := find(Run(context.Background(), env, DefaultRequirements(), Options{}), "Existing: VM density"); got != nil {
		t.Errorf("nothing set: %+v", got)
	}
	hc := obj(cluster.GVRHyperConverged, "HyperConverged", cluster.CNVNamespace, cluster.HyperConvergedName, map[string]interface{}{
		"spec": map[string]interface{}{"higherWorkloadDensity": map[string]interface{}{"memoryOvercommitPercentage": int64(150)}}})
	if err := env.Dyn.(*dynfake.FakeDynamicClient).Tracker().Create(cluster.GVRHyperConverged, hc, cluster.CNVNamespace); err != nil {
		t.Fatal(err)
	}
	got := find(Run(context.Background(), env, DefaultRequirements(), Options{}), "Existing: VM density")
	if got == nil || got.Status != Info || !strings.Contains(got.Detail, "memory overcommit 150%, KSM off") {
		t.Errorf("density = %+v", got)
	}
}

func TestWorkerNames(t *testing.T) {
	cordoned := node("cordoned", "worker", 90)
	cordoned.Spec.Unschedulable = true
	c := kfake.NewClientset(
		node("worker-b", "worker", 90),
		node("worker-a", "worker", 90),
		node("control", "control-plane", 90),
		cordoned,
	)
	got, err := WorkerNames(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"worker-a", "worker-b"}; !slices.Equal(got, want) {
		t.Errorf("WorkerNames = %v, want %v", got, want)
	}
}

// A raw Ceph OSD has no partition table, no filesystem and no holder, so only
// the signatures and the data read keep it from being called empty.
func TestRunRejectsCephOSDDisk(t *testing.T) {
	r := Run(context.Background(), labEnv(t, "probe-ceph-osd-only.txt"), DefaultRequirements(), Options{})
	if r.Suggested.Node != "" || r.Suggested.Disk != "" {
		t.Fatalf("no node or disk should be suggested, got %+v", r.Suggested)
	}
	got := find(r, "bm-worker: disk")
	if got == nil || got.Status != Fail {
		t.Fatalf("bm-worker: disk = %+v", got)
	}
	for _, want := range []string{"sdb 1919GB", "ceph_bluestore", "the first MiB holds data"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q missing %q", got.Detail, want)
		}
	}
}

func TestRunPicksBlankDiskNextToCephOSD(t *testing.T) {
	r := Run(context.Background(), labEnv(t, "probe-ceph-osd-and-blank.txt"), DefaultRequirements(), Options{})
	if want := "/dev/disk/by-id/wwn-0x5000000000000003"; r.Suggested.Disk != want {
		t.Fatalf("Suggested.Disk = %q, want the blank sdc %q", r.Suggested.Disk, want)
	}
	got := find(r, "bm-worker: disk")
	if got == nil || got.Status != Pass || !strings.Contains(got.Detail, "in use: sdb") {
		t.Fatalf("bm-worker: disk = %+v, want the OSD named as in use", got)
	}
}

func TestRunRejectsHeldMountedAndUnreadableDisks(t *testing.T) {
	cases := []struct{ fixture, want string }{
		{"probe-multipath.txt", "held by dm-0"},
		{"probe-host-mounted.txt", "mounted at /mnt/local-storage/localblock-sc/wwn-0x5000000000000002 on the node"},
		{"probe-unreadable-disk.txt", "the first MiB could not be read"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			r := Run(context.Background(), labEnv(t, c.fixture), DefaultRequirements(), Options{})
			got := find(r, "bm-worker: disk")
			if got == nil || got.Status != Fail || !strings.Contains(got.Detail, c.want) {
				t.Fatalf("bm-worker: disk = %+v, want %q", got, c.want)
			}
		})
	}
}

// A local PV owns a raw block device no host probe can tell is used.
func TestRunRejectsDiskWithBoundLocalPV(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "local-pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			Local:    &corev1.LocalVolumeSource{Path: "/mnt/local-storage/localblock-sc/wwn-0x5000000000000002"},
			ClaimRef: &corev1.ObjectReference{Name: "ocs-deviceset-0-data"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	r := Run(context.Background(), labEnv(t, "probe-baremetal-empty-disk.txt", pv), DefaultRequirements(), Options{})
	got := find(r, "bm-worker: disk")
	if got == nil || got.Status != Fail || !strings.Contains(got.Detail, "claimed by local PV local-pv-1") {
		t.Fatalf("bm-worker: disk = %+v, want the bound PV named", got)
	}
	if r.Suggested.Disk != "" {
		t.Errorf("a disk with a bound PV must not be suggested, got %q", r.Suggested.Disk)
	}
}

// An Available or Released local PV still means LSO owns the device path.
func TestRunRejectsDiskWithUnboundLocalPV(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "local-pv-leftover"},
		Spec: corev1.PersistentVolumeSpec{
			Local: &corev1.LocalVolumeSource{Path: "/mnt/local-storage/localblock-sc/wwn-0x5000000000000002"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
	r := Run(context.Background(), labEnv(t, "probe-baremetal-empty-disk.txt", pv), DefaultRequirements(), Options{})
	got := find(r, "bm-worker: disk")
	if got == nil || got.Status != Fail || !strings.Contains(got.Detail, "referenced by local PV local-pv-leftover") {
		t.Fatalf("bm-worker: disk = %+v, want the unbound PV named", got)
	}
}

func TestRunReportsDegradedProbe(t *testing.T) {
	r := Run(context.Background(), labEnv(t, "probe-degraded.txt"), DefaultRequirements(), Options{})
	got := find(r, "bm-worker: probe")
	if got == nil || got.Status != Warn || !strings.Contains(got.Detail, "lsblk") {
		t.Fatalf("bm-worker: probe = %+v, want a warn naming lsblk", got)
	}
}
