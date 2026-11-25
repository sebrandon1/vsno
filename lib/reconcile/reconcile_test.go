package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
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

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// world is a fake environment: a namespace, a ready builder and the registry
// credentials cache.Reconcile needs.
func newWorld(t *testing.T, dynObjs ...runtime.Object) (*kfake.Clientset, *dynfake.FakeDynamicClient, Env, *guestctltest.Fake) {
	t.Helper()
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "builder-1", Labels: map[string]string{"app": "builder"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	k := kfake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		ready,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"},
			Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"},
			Data: map[string][]byte{"ca.crt": []byte("ca")}},
	)
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cluster.GVRVirtualMachine: "VirtualMachineList", guest.GVRDataVolume: "DataVolumeList",
	}, dynObjs...)
	acc := &guestctltest.Fake{}
	e := Env{
		Env: seed.Env{
			Env: guest.Env{
				Kube: k, Dyn: d, Progress: precheck.NoProgress(),
				Now: func() time.Time { return t0 }, Poll: time.Millisecond, Timeout: time.Second,
			},
			Access: func(string) seed.Access { return acc },
			Volume: func(context.Context, string) (int64, int64, error) { return 0, 100 << 30, nil },
		},
		Cache: cache.Env{Kube: k, Dyn: d, Now: func() time.Time { return t0 }},
	}
	return k, d, e, acc
}

func vmObj(name string, ann map[string]string) *unstructured.Unstructured {
	a := map[string]any{}
	for k, v := range ann {
		a[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{guest.LabelName: name}, "annotations": a},
	}}
}

func oldService(name string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, Labels: map[string]string{guest.LabelName: name},
		CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour)),
	}}
}

func svcObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "namespace": ns},
	}}
}

func sweep(t *testing.T, e Env, o Options) *Report {
	t.Helper()
	o.Namespace = ns
	rep, err := Sweep(context.Background(), e, o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func ofKind(rep *Report, kind string) []Item {
	var out []Item
	for _, it := range rep.Items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

func TestSweepAHealthyEnvironmentFindsNothing(t *testing.T) {
	_, _, e, _ := newWorld(t)
	rep := sweep(t, e, Options{})
	if len(rep.Items) != 0 || rep.Failed() {
		t.Errorf("items = %+v", rep.Items)
	}
}

func TestSweepReportsAMirror(t *testing.T) {
	k, _, e, _ := newWorld(t)
	_, err := k.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: cache.ConfigMap},
		Data:       map[string]string{"cache.json": `{"entries":{"sha256:aa":{"version":"4.20.46","digest":"sha256:aa","state":"mirroring"}}}`},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rep := sweep(t, e, Options{})
	it := ofKind(rep, "cache mirror")
	if len(it) != 1 || it[0].Name != "4.20.46" || it[0].Action != Observe || it[0].Done {
		t.Fatalf("items = %+v", rep.Items)
	}
	// Report-only changes nothing.
	if pending, _ := cache.Pending(context.Background(), k, ns); len(pending) != 1 {
		t.Errorf("a report-only sweep settled the mirror")
	}
}

func TestSweepPrunesAnAbandonedService(t *testing.T) {
	k, d, e, _ := newWorld(t, svcObj("old"))
	if _, err := k.CoreV1().Services(ns).Create(context.Background(), oldService("old"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rep := sweep(t, e, Options{Prune: true})
	it := ofKind(rep, "abandoned vSNO")
	if len(it) != 1 || !it[0].Done || it[0].Action != Delete || rep.Failed() {
		t.Fatalf("items = %+v", rep.Items)
	}
	// guest.Delete removes the Service through the dynamic client; the typed
	// copy here is only how AbandonedServices found it, since the fake clients
	// do not share objects the way one API server does.
	if _, err := d.Resource(cluster.GVRService).Namespace(ns).Get(context.Background(), "old", metav1.GetOptions{}); err == nil {
		t.Error("the Service is still there")
	}
}

func TestSweepLeavesAYoungService(t *testing.T) {
	k, _, e, _ := newWorld(t, svcObj("new"))
	s := oldService("new")
	s.CreationTimestamp = metav1.NewTime(t0.Add(-time.Minute))
	if _, err := k.CoreV1().Services(ns).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rep := sweep(t, e, Options{Prune: true})
	if len(ofKind(rep, "abandoned vSNO")) != 0 {
		t.Errorf("items = %+v", rep.Items)
	}
	if _, err := k.CoreV1().Services(ns).Get(context.Background(), "new", metav1.GetOptions{}); err != nil {
		t.Errorf("a building create was deleted: %v", err)
	}
}

func TestSweepPrunesAnOrphanDNSKey(t *testing.T) {
	k, _, e, _ := newWorld(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: manifests.GuestsConfigMap},
		Data: map[string]string{"ghost.conf": "address=/api.ghost/1.2.3.4"}}
	if _, err := k.CoreV1().ConfigMaps(ns).Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rep := sweep(t, e, Options{Prune: true})
	it := ofKind(rep, "DNS entry")
	if len(it) != 1 || !it[0].Done {
		t.Fatalf("items = %+v", rep.Items)
	}
	got, _ := k.CoreV1().ConfigMaps(ns).Get(context.Background(), manifests.GuestsConfigMap, metav1.GetOptions{})
	if _, ok := got.Data["ghost.conf"]; ok {
		t.Error("the DNS key is still there")
	}
}

func TestSweepKeepsDNSForAService(t *testing.T) {
	k, _, e, _ := newWorld(t, svcObj("keep"))
	if _, err := k.CoreV1().Services(ns).Create(context.Background(), oldService("keep"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: manifests.GuestsConfigMap},
		Data:       map[string]string{"keep.conf": "x"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rep := sweep(t, e, Options{})
	if len(ofKind(rep, "DNS entry")) != 0 {
		t.Errorf("items = %+v", rep.Items)
	}
}

func TestSweepClearsAFinishedSeedMark(t *testing.T) {
	_, d, e, _ := newWorld(t, vmObj("seed", map[string]string{guest.AnnSeeding: t0.Add(-time.Minute).Format(time.RFC3339)}))
	rep := sweep(t, e, Options{Prune: true})
	it := ofKind(rep, "seed mark")
	if len(it) != 1 || !it[0].Done {
		t.Fatalf("items = %+v", rep.Items)
	}
	vm, _ := d.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "seed", metav1.GetOptions{})
	if vm.GetAnnotations()[guest.AnnSeeding] != "" {
		t.Errorf("the mark is still there: %v", vm.GetAnnotations())
	}
}

func TestSweepKeepsASeedMarkItCannotVerify(t *testing.T) {
	_, d, e, acc := newWorld(t, vmObj("seed", map[string]string{guest.AnnSeeding: t0.Format(time.RFC3339)}))
	acc.OcReplies = map[string][]guestctltest.Reply{
		"get seedgenerators seedimage --ignore-not-found --request-timeout=15s": {{Err: errors.New("the guest is not answering")}},
	}
	rep := sweep(t, e, Options{Prune: true})
	it := ofKind(rep, "seed mark")
	if len(it) != 1 || it[0].Done {
		t.Fatalf("items = %+v", rep.Items)
	}
	vm, _ := d.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "seed", metav1.GetOptions{})
	if vm.GetAnnotations()[guest.AnnSeeding] == "" {
		t.Error("the mark was cleared while the guest could not be read")
	}
}

func TestSweepReportsTheRegistrysRoom(t *testing.T) {
	_, _, e, _ := newWorld(t)
	e.Volume = func(context.Context, string) (int64, int64, error) { return 100 << 30, 100 << 30, nil }
	rep := sweep(t, e, Options{})
	if len(ofKind(rep, "environment")) == 0 {
		t.Errorf("items = %+v", rep.Items)
	}
}

func TestSweepStopsWhenAnotherReconcileHoldsTheLease(t *testing.T) {
	k, _, e, _ := newWorld(t)
	at := metav1.NewMicroTime(t0)
	secs := int32(60)
	who := "elsewhere:1"
	if _, err := k.CoordinationV1().Leases(ns).Create(context.Background(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: LeaseName},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &who, LeaseDurationSeconds: &secs, RenewTime: &at},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rep := sweep(t, e, Options{Prune: true})
	if len(rep.Items) != 1 || rep.Items[0].Kind != "reconcile" {
		t.Fatalf("items = %+v", rep.Items)
	}
	if _, err := k.CoordinationV1().Leases(ns).Get(context.Background(), LeaseName, metav1.GetOptions{}); err != nil {
		t.Errorf("the other holder's lease was deleted: %v", err)
	}
}

func TestSweepContainsACacheError(t *testing.T) {
	k, _, e, _ := newWorld(t)
	if _, err := k.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: cache.ConfigMap},
		Data:       map[string]string{"cache.json": `{"entries":{"sha256:aa":{"version":"4.20.46","digest":"sha256:aa","state":"mirroring"}}}`},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	k.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the registry is unreachable")
	})
	rep := sweep(t, e, Options{Prune: true})
	it := ofKind(rep, "cache mirror")
	if len(it) != 1 || it[0].Error == "" || !rep.Failed() {
		t.Fatalf("items = %+v", rep.Items)
	}
	if pending, _ := cache.Pending(context.Background(), k, ns); len(pending) != 1 {
		t.Errorf("the sweep lied about settling the mirror")
	}
}

// A read failure is an Error item, not a warning, and does not stop later
// steps: a cron run's exit code has to mean "the sweep broke".
func TestSweepReportsAReadFailure(t *testing.T) {
	_, d, e, _ := newWorld(t)
	d.PrependReactor("list", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is restarting")
	})
	rep := sweep(t, e, Options{})
	if !rep.Failed() {
		t.Fatalf("a read failure did not fail the sweep: %+v", rep.Items)
	}
	if len(ofKind(rep, "abandoned vSNO")) == 0 || len(ofKind(rep, "seed mark")) == 0 {
		t.Errorf("later steps did not run: %+v", rep.Items)
	}
}

// A namespace-scoped user without leases still gets a sweep: the lock is
// advisory, so it warns and carries on unlocked.
func TestSweepContinuesWhenLeasesAreForbidden(t *testing.T) {
	k, d, e, _ := newWorld(t, svcObj("old"))
	if _, err := k.CoreV1().Services(ns).Create(context.Background(), oldService("old"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, LeaseName, errors.New("no leases for you"))
	for _, verb := range []string{"get", "create"} {
		k.PrependReactor(verb, "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
	}
	rep := sweep(t, e, Options{Prune: true})
	if len(ofKind(rep, "reconcile")) != 1 {
		t.Errorf("no advisory item: %+v", rep.Items)
	}
	it := ofKind(rep, "abandoned vSNO")
	if len(it) != 1 || !it[0].Done {
		t.Errorf("the sweep stopped on a forbidden lease: %+v", rep.Items)
	}
	if _, err := d.Resource(cluster.GVRService).Namespace(ns).Get(context.Background(), "old", metav1.GetOptions{}); err == nil {
		t.Error("the abandoned Service was not removed")
	}
}

func TestSweepReportsAMissingNamespace(t *testing.T) {
	_, _, e, _ := newWorld(t)
	if _, err := Sweep(context.Background(), e, Options{Namespace: "gone"}); err == nil {
		t.Error("want an error for a namespace that does not exist")
	}
}
