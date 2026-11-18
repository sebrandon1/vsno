package precheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	return obj(GVRPackageManifest, "PackageManifest", marketplaceNS, name,
		map[string]interface{}{"status": map[string]interface{}{"defaultChannel": def, "channels": chs}})
}

func newDyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{
		GVRClusterVersion: "ClusterVersionList", GVRNetwork: "NetworkList",
		GVRPackageManifest: "PackageManifestList", GVRCSV: "ClusterServiceVersionList",
		GVRHyperConverged: "HyperConvergedList", GVRLVMCluster: "LVMClusterList",
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
		if r.Results[i].Check == check {
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
			t.Logf("%-5s %-40s %s", res.Status, res.Check, res.Detail)
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
		if res.Check == "bm-worker: memory" && res.Status == Info && strings.Contains(res.Detail, "66 GiB held by the existing vsno") {
			held = true
		}
	}
	if !held {
		t.Error("want an info row explaining memory held by the existing environment")
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
