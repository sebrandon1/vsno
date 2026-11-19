package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
