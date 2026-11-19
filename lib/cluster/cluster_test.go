package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/sebrandon1/vsno/lib/precheck"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func csv(ns, name, phase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"phase": phase}}}
	u.SetAPIVersion("operators.coreos.com/v1alpha1")
	u.SetKind("ClusterServiceVersion")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func fakeDyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{precheck.GVRCSV: "ClusterServiceVersionList"}, objs...)
}

func TestSucceededCSV(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn(
		csv(CNVNamespace, "kubevirt-hyperconverged-operator.v1", "Replacing"),
		csv(CNVNamespace, "kubevirt-hyperconverged-operator.v2", "Succeeded"),
		csv(CNVNamespace, "other-operator.v1", "Succeeded"),
		csv(StorageNamespace, "lvms-operator.v1", "Installing"),
	)
	if got, err := SucceededCSV(ctx, dyn, CNV); err != nil || got != "kubevirt-hyperconverged-operator.v2" {
		t.Errorf("cnv = %q %v", got, err)
	}
	if got, err := SucceededCSV(ctx, dyn, LVMS); err != nil || got != "" {
		t.Errorf("lvms = %q %v, want none succeeded", got, err)
	}
	csvs, _ := CSVs(ctx, dyn, CNV)
	if len(csvs) != 2 {
		t.Errorf("CSVs(cnv) = %d, want 2 (other operators filtered out)", len(csvs))
	}

	dyn.PrependReactor("list", "clusterserviceversions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if _, err := SucceededCSV(ctx, dyn, CNV); err == nil {
		t.Error("want the list error")
	}
}

func TestCondition(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": []any{
		"not a map",
		map[string]any{"type": "Degraded", "status": "False"},
		map[string]any{"type": "Available", "status": "True"},
		map[string]any{"type": "Odd", "status": int64(1)},
	}}}}
	for typ, want := range map[string]string{"Available": "True", "Degraded": "False", "Odd": "Unknown", "Missing": "Unknown"} {
		if got := Condition(u, typ); got != want {
			t.Errorf("%s = %q, want %q", typ, got, want)
		}
	}
	if got := Condition(&unstructured.Unstructured{Object: map[string]any{}}, "Available"); got != "Unknown" {
		t.Errorf("no status = %q", got)
	}
}

func TestRolledOutAndReady(t *testing.T) {
	d := func(gen, replicas, observed, updated, available int64, setReplicas bool) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{
			"observedGeneration": observed, "updatedReplicas": updated, "availableReplicas": available}}}
		if setReplicas {
			_ = unstructured.SetNestedField(u.Object, replicas, "spec", "replicas")
		}
		u.SetGeneration(gen)
		return u
	}
	cases := []struct {
		name             string
		d                *unstructured.Unstructured
		want             bool
		available, wantN int64
	}{
		{"ready", d(2, 1, 2, 1, 1, true), true, 1, 1},
		{"default replicas", d(1, 0, 1, 1, 1, false), true, 1, 1},
		{"old generation", d(3, 1, 2, 1, 1, true), false, 1, 1},
		{"not available", d(1, 1, 1, 1, 0, true), false, 0, 1},
		{"not updated", d(1, 2, 1, 1, 2, true), false, 2, 2},
		{"scaled to zero", d(1, 0, 1, 0, 0, true), true, 0, 0},
	}
	for _, c := range cases {
		if got := RolledOut(c.d); got != c.want {
			t.Errorf("%s: RolledOut = %v", c.name, got)
		}
		if a, w := Ready(c.d); a != c.available || w != c.wantN {
			t.Errorf("%s: Ready = %d/%d", c.name, a, w)
		}
	}
}
