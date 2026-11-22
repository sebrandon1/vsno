package snapshot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const ns = "vsno-test"

// applier creates what it is given, and marks a snapshot ready like the storage
// driver would.
type applier struct{ dyn *dynfake.FakeDynamicClient }

func (a applier) Apply(_ context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	for _, o := range objs {
		_ = unstructured.SetNestedField(o.Object, true, "status", "readyToUse")
		_ = unstructured.SetNestedField(o.Object, "200Gi", "status", "restoreSize")
		if err := a.dyn.Tracker().Create(cluster.GVRVolumeSnapshot, o.DeepCopy(), o.GetNamespace()); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// runner records the scripts run in the builder.
type runner struct{ ran []string }

func (r *runner) Run(_ context.Context, script string, args []string, _ builder.IO) error {
	r.ran = append(r.ran, script+" "+strings.Join(args, " "))
	return nil
}

func vmObj(name, status, version string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"runStrategy": "Always"},
		"status": map[string]any{
			"printableStatus": status,
		},
	}}
	u.SetAPIVersion("kubevirt.io/v1")
	u.SetKind("VirtualMachine")
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{guest.LabelName: name})
	if version != "" {
		u.SetAnnotations(map[string]string{guest.AnnVersion: version})
	}
	return u
}

func snapObj(name, vsno, version string, ready bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"volumeSnapshotClassName": "lvms-vg-vsno",
			"source":                  map[string]any{"persistentVolumeClaimName": name + "-root"},
		},
		"status": map[string]any{"restoreSize": "200Gi", "readyToUse": ready},
	}}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshot")
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{cluster.ManagedByLabel: "vsno", LabelVSNO: vsno})
	if version != "" {
		u.SetAnnotations(map[string]string{guest.AnnVersion: version})
	}
	u.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)))
	return u
}

func testEnv(t *testing.T, objs ...runtime.Object) (Env, *dynfake.FakeDynamicClient, *runner) {
	t.Helper()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cluster.GVRVolumeSnapshot: "VolumeSnapshotList",
		cluster.GVRVirtualMachine: "VirtualMachineList",
		guest.GVRDataVolume:       "DataVolumeList",
		cluster.GVRVMI:            "VirtualMachineInstanceList",
	}, objs...)
	r := &runner{}
	var svcs []runtime.Object
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "VirtualMachine" {
			svcs = append(svcs, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: u.GetName()},
				Spec: corev1.ServiceSpec{ClusterIP: "172.30.0.9"}})
		}
	}
	kube := kfake.NewClientset(svcs...)
	now := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	return Env{Env: guest.Env{Kube: kube, Dyn: dyn, Applier: applier{dyn}, Builder: r,
		Progress: precheck.NoProgress(), Poll: time.Millisecond, Timeout: time.Second,
		Now: func() time.Time { now = now.Add(time.Second); return now }}}, dyn, r
}

func TestListAndUse(t *testing.T) {
	ctx := context.Background()
	e, _, _ := testEnv(t, vmObj("seed", "Running", "4.22.22"), snapObj("seed", "seed", "4.22.22", true),
		snapObj("gone", "gone", "4.21.1", true))
	got, err := List(ctx, e, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %+v", got)
	}
	byName := map[string]Entry{}
	for _, en := range got {
		byName[en.Name] = en
	}
	if s := byName["seed"]; !s.InUse || s.Version != "4.22.22" || s.VSNO != "seed" || s.SizeBytes != 200<<30 {
		t.Errorf("seed = %+v", s)
	}
	if s := byName["gone"]; s.InUse {
		t.Errorf("gone is in use, but its vSNO is absent: %+v", s)
	}
	one, err := Use(ctx, e, ns, "seed")
	if err != nil || one == nil || one.Version != "4.22.22" {
		t.Errorf("use = %+v, err = %v", one, err)
	}
	if none, err := Use(ctx, e, ns, "none"); none != nil || err != nil {
		t.Errorf("use of a missing snapshot = %+v, err = %v", none, err)
	}
}

func TestUseRefusesOneThatIsNotReady(t *testing.T) {
	e, _, _ := testEnv(t, snapObj("seed", "seed", "4.22.22", false))
	if _, err := Use(context.Background(), e, ns, "seed"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Errorf("err = %v", err)
	}
}

// A snapshot stops the vSNO, marks it ready, keeps the installer files and
// starts the vSNO again.
func TestCreateStopsAndRestarts(t *testing.T) {
	ctx := context.Background()
	e, dyn, r := testEnv(t, vmObj("seed", "Stopped", "4.22.22"))
	ent, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if ent.Name != "seed" || ent.Version != "4.22.22" || ent.SizeBytes != 200<<30 {
		t.Errorf("entry = %+v", ent)
	}
	if len(r.ran) != 1 || !strings.Contains(r.ran[0], "cp -a") || !strings.HasSuffix(r.ran[0], " seed") {
		t.Errorf("builder ran %v, want the installer files copied", r.ran)
	}
	u, err := dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns).Get(ctx, "seed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if u.GetAnnotations()[guest.AnnVersion] != "4.22.22" || u.GetLabels()[LabelVSNO] != "seed" {
		t.Errorf("snapshot = %v / %v", u.GetLabels(), u.GetAnnotations())
	}
	if !strings.Contains(u.Object["spec"].(map[string]any)["volumeSnapshotClassName"].(string), "lvms") {
		t.Errorf("class = %v", u.Object["spec"])
	}
}

// Snapshotting a vSNO that already has one replaces it, so the data is a new
// point in time rather than the old content kept by an update.
func TestCreateReplacesAnExistingSnapshot(t *testing.T) {
	ctx := context.Background()
	e, dyn, _ := testEnv(t, vmObj("seed", "Stopped", "4.22.22"), snapObj("seed", "seed", "4.21.1", true))
	if _, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "seed"}); err != nil {
		t.Fatal(err)
	}
	u, err := dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns).Get(ctx, "seed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.GetAnnotations()[guest.AnnVersion]; got != "4.22.22" {
		t.Errorf("version = %q, want the vSNO's current 4.22.22", got)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		dryRun bool
		gone   bool
		ran    bool
	}{{"dry run keeps it", true, false, false}, {"delete removes it", false, true, true}} {
		t.Run(c.name, func(t *testing.T) {
			e, dyn, r := testEnv(t, snapObj("seed", "seed", "4.22.22", true))
			res, err := Delete(ctx, e, DeleteOptions{Namespace: ns, Names: []string{"seed"}, DryRun: c.dryRun, Yes: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Snapshots) != 1 || res.Snapshots[0].Name != "seed" {
				t.Errorf("result = %+v", res)
			}
			_, err = dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns).Get(ctx, "seed", metav1.GetOptions{})
			if gone := apierrors.IsNotFound(err); gone != c.gone {
				t.Errorf("gone = %v, err = %v", gone, err)
			}
			if ran := len(r.ran) > 0; ran != c.ran {
				t.Errorf("builder ran %v", r.ran)
			}
		})
	}
}

func TestDeleteNamesMissingThenAsks(t *testing.T) {
	e, _, _ := testEnv(t, snapObj("seed", "seed", "4.22.22", true))
	if _, err := Delete(context.Background(), e, DeleteOptions{Namespace: ns, Names: []string{"none"}, Yes: true}); err == nil {
		t.Error("expected an error for a missing snapshot")
	}
	// Without --yes and without a terminal to ask on, nothing is deleted.
	if _, err := Delete(context.Background(), e, DeleteOptions{Namespace: ns, Names: []string{"seed"}}); err == nil {
		t.Error("expected the question to refuse")
	}
}
