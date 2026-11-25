package state

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func TestLoadMissing(t *testing.T) {
	st, found, err := Load(context.Background(), fake.NewClientset(), "vsno")
	if err != nil || found || st == nil || st.Node != "" {
		t.Fatalf("got %+v %v %v", st, found, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	k := fake.NewClientset()
	st := &State{Node: "worker-1", Disk: "/dev/disk/by-id/wwn-0x5000000000000002", Services: map[string]string{"registry": "172.30.1.2"}}
	st.Record("setup", Running, "", t0)
	if err := Save(ctx, k, "vsno", st); err != nil {
		t.Fatal(err)
	}
	st.Record("setup", Done, "4 support pods ready", t0.Add(time.Minute))
	if err := Save(ctx, k, "vsno", st); err != nil { // update path
		t.Fatal(err)
	}

	got, found, err := Load(ctx, k, "vsno")
	if err != nil || !found {
		t.Fatalf("load: %v %v", found, err)
	}
	if got.Node != st.Node || got.Services["registry"] != "172.30.1.2" || len(got.Stages) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	last, ok := got.Last("setup")
	if !ok || last.Status != Done || !last.Time.Equal(t0.Add(time.Minute)) {
		t.Errorf("Last(setup) = %+v %v", last, ok)
	}
	if _, ok := got.Last("upgrade"); ok {
		t.Error("Last of an unrecorded stage should be false")
	}
	cm, _ := k.CoreV1().ConfigMaps("vsno").Get(ctx, ConfigMapName, metav1.GetOptions{})
	if cm.Labels["app.kubernetes.io/managed-by"] != "vsno" {
		t.Errorf("labels = %v", cm.Labels)
	}
}

func TestSaveFillsNilData(t *testing.T) {
	ctx := context.Background()
	k := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "vsno"}})
	if err := Save(ctx, k, "vsno", &State{Node: "n"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := Load(ctx, k, "vsno"); got.Node != "n" {
		t.Errorf("node = %q", got.Node)
	}
}

func TestRecordKeepsRecentHistory(t *testing.T) {
	st := &State{}
	for i := range maxStages + 5 {
		st.Record("s", Done, "", t0.Add(time.Duration(i)*time.Second))
	}
	if len(st.Stages) != maxStages || !st.Stages[0].Time.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("len=%d first=%v", len(st.Stages), st.Stages[0].Time)
	}
}

func TestLoadErrors(t *testing.T) {
	ctx := context.Background()
	bad := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "vsno"}, Data: map[string]string{key: "{"}})
	if _, found, err := Load(ctx, bad, "vsno"); err == nil || !found {
		t.Errorf("corrupt state: found=%v err=%v", found, err)
	}

	k := fake.NewClientset()
	k.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if _, _, err := Load(ctx, k, "vsno"); err == nil || !strings.Contains(err.Error(), "api down") {
		t.Errorf("want api error, got %v", err)
	}
	if err := Save(ctx, k, "vsno", &State{}); err == nil || !strings.Contains(err.Error(), "writing vsno/vsno-state") {
		t.Errorf("want wrapped save error, got %v", err)
	}
}

func TestSaveUpdateError(t *testing.T) {
	k := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "vsno"}})
	k.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("conflict")
	})
	if err := Save(context.Background(), k, "vsno", &State{}); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("want update error, got %v", err)
	}
}

func TestMutateRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	k := fake.NewClientset()
	if err := Save(ctx, k, "vsno", &State{Node: "worker-0"}); err != nil {
		t.Fatal(err)
	}
	var updates int
	k.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if updates == 0 {
			updates++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, ConfigMapName, errors.New("changed"))
		}
		return false, nil, nil
	})
	if err := Mutate(ctx, k, "vsno", func(st *State) { st.HostDNS = "10.0.0.2" }); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(ctx, k, "vsno")
	if updates != 1 || got.HostDNS != "10.0.0.2" || got.Node != "worker-0" {
		t.Errorf("updates=%d state=%+v", updates, got)
	}
}

func TestMutateRetriesOnAlreadyExists(t *testing.T) {
	ctx := context.Background()
	k := fake.NewClientset()
	var creates int
	k.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if creates == 0 {
			creates++
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, ConfigMapName)
		}
		return false, nil, nil
	})
	if err := Mutate(ctx, k, "vsno", func(st *State) { st.Node = "worker-1" }); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := Load(ctx, k, "vsno"); creates != 1 || got.Node != "worker-1" {
		t.Errorf("creates=%d state=%+v", creates, got)
	}
}

// Two writers editing different fields both land: each Mutate reloads the
// previous writer's save instead of overwriting it with an older copy.
func TestMutateMergesDifferentFields(t *testing.T) {
	ctx := context.Background()
	k := fake.NewClientset()
	if err := Save(ctx, k, "vsno", &State{Node: "worker-0"}); err != nil {
		t.Fatal(err)
	}
	if err := Mutate(ctx, k, "vsno", func(st *State) { st.Services = map[string]string{"registry": "1.2.3.4"} }); err != nil {
		t.Fatal(err)
	}
	if err := Mutate(ctx, k, "vsno", func(st *State) { st.RecordSync(Sync{Ref: "r/x:1", SyncedAt: t0}) }); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(ctx, k, "vsno")
	if got.Services["registry"] != "1.2.3.4" || got.Node != "worker-0" {
		t.Errorf("earlier fields lost: %+v", got)
	}
	if _, ok := got.Synced("r/x:1"); !ok {
		t.Errorf("sync lost: %+v", got.Syncs)
	}
}

func TestMutateKeepsRecordCap(t *testing.T) {
	ctx := context.Background()
	k := fake.NewClientset()
	st := &State{}
	for i := range maxStages + 10 {
		st.Stages = append(st.Stages, Stage{Name: "s", Status: Done, Time: t0.Add(time.Duration(i) * time.Second)})
	}
	if err := Save(ctx, k, "vsno", st); err != nil {
		t.Fatal(err)
	}
	newest := t0.Add(time.Hour)
	if err := Mutate(ctx, k, "vsno", func(st *State) { st.Record("last", Done, "", newest) }); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(ctx, k, "vsno")
	if len(got.Stages) != maxStages {
		t.Fatalf("len = %d", len(got.Stages))
	}
	if last := got.Stages[len(got.Stages)-1]; last.Name != "last" || !last.Time.Equal(newest) {
		t.Errorf("newest entry = %+v", last)
	}
}

func TestDensityText(t *testing.T) {
	for _, c := range []struct {
		d    Density
		want string
	}{
		{Density{}, "memory overcommit 100%, KSM off"},
		{Density{OvercommitPercent: 150, KSMNode: "worker-0"}, "memory overcommit 150%, KSM on worker-0"},
		{Density{KSMNode: "*"}, "memory overcommit 100%, KSM on (every node)"},
	} {
		if got := c.d.String(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestSyncsRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	st := &State{}
	st.RecordSync(Sync{Ref: "r/seeds/a:4.20.46", Digest: "sha256:aa", SyncedAt: at})
	st.RecordSync(Sync{Ref: "r/seeds/b:4.20.46", SyncedAt: at.Add(time.Hour)})
	st.RecordSync(Sync{Ref: "r/seeds/b:4.20.46", SyncedAt: at.Add(2 * time.Hour)}) // a newer copy wins
	st.DropSync("r/seeds/a:4.20.46")
	if _, ok := st.Synced("r/seeds/a:4.20.46"); ok {
		t.Error("dropped entry is still there")
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got, ok := back.Synced("r/seeds/b:4.20.46")
	if !ok || !got.SyncedAt.Equal(at.Add(2*time.Hour)) {
		t.Errorf("got %+v, ok = %v", got, ok)
	}
}
