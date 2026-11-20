package guest

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestList(t *testing.T) {
	w := newWorld(t,
		vm("ready", "Running"), dv("ready-iso", "Succeeded", "100.0%"),
		vm("new", "Provisioning"), dv("new-iso", "ImportInProgress", "12.50%"),
		vm("off", "Stopped"),
	)
	for _, s := range []*corev1.Service{svc("ready", "172.30.1.1"), svc("new", "172.30.1.2"), svc("off", "172.30.1.3")} {
		if _, err := w.kube.CoreV1().Services(ns).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	w.runner.outputs["probe.sh"] = probeLine("ready", `"cv":{"version":"4.20.47","available":true},"unhealthy":1,"ibu":{"stage":"Prep","completed":true}`) + "not json\n"
	got, err := List(context.Background(), w.env(), ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "new" || got[1].Name != "off" || got[2].Name != "ready" {
		t.Fatalf("list = %+v", got)
	}
	r := got[2]
	r.Created, r.Image = time.Time{}, ""
	want := Guest{Namespace: ns, Name: "ready", State: Ready, Detail: "1 operator(s) degraded or unavailable", Version: "4.20.47", Requested: "4.20",
		Node: node, Cores: 16, MemoryGiB: 32, DiskGiB: 200, ServiceIP: "172.30.1.1", API: "https://api.ready.vsno.lab:6443", Unhealthy: intp(1), IBU: "Prep done"}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("ready =\n %+v\nwant\n %+v", r, want)
	}
	if got[0].State != Importing || got[0].Detail != "12.50%" || got[1].State != Stopped {
		t.Errorf("new = %+v, off = %+v", got[0], got[1])
	}
	// Only the running vSNO is probed.
	if p := w.runner.ran("probe.sh"); len(p) != 1 || strings.Join(p[0].args, " ") != "vsno.lab ready=172.30.1.1" {
		t.Errorf("probes = %+v", p)
	}
}

func TestEnvironments(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// A hand-built environment: no vsno label, but it has a builder.
	if _, err := w.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "hand-built"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.kube.CoreV1().Pods("hand-built").Create(ctx,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "hand-built", Name: "builder-2", Labels: map[string]string{"app": "builder"}}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// An unrelated namespace, and a labeled one with no builder.
	for _, o := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "labeled-only", Labels: map[string]string{cluster.ManagedByLabel: "vsno"}}},
	} {
		if _, err := w.kube.CoreV1().Namespaces().Create(ctx, o, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Environments(context.Background(), w.kube)
	if err != nil {
		t.Fatal(err)
	}
	// ns is both labeled and has a builder: once.
	if want := []string{"hand-built", "labeled-only", ns}; !reflect.DeepEqual(got, want) {
		t.Errorf("Environments = %v, want %v", got, want)
	}
	w.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if _, err := Environments(context.Background(), w.kube); err == nil || !strings.Contains(err.Error(), "finding the builders") {
		t.Errorf("err = %v", err)
	}
}

func TestListAll(t *testing.T) {
	w := newWorld(t, vm("b", "Running"), vm("a", "Stopped"))
	other := vm("c", "Stopped")
	other.SetNamespace("other")
	if _, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace("other").Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.kube.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.kube.CoreV1().Pods("other").Create(context.Background(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "builder-3", Labels: map[string]string{"app": "builder"}}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := ListAll(context.Background(), w.kube, func(string) (Env, error) { return w.env(), nil })
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, g := range got {
		keys = append(keys, g.Namespace+"/"+g.Name)
	}
	if want := []string{"other/c", ns + "/a", ns + "/b"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("ListAll = %v, want %v", keys, want)
	}
}

func TestListShowsISOBuilds(t *testing.T) {
	w := newWorld(t)
	s := svc("new", "172.30.1.9")
	s.Labels = map[string]string{LabelName: "new"}
	s.Annotations = map[string]string{AnnRequested: "4.20", AnnVersion: "4.20.46"}
	if _, err := w.kube.CoreV1().Services(ns).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := List(context.Background(), w.env(), ns)
	if err != nil || len(got) != 1 || got[0].State != Building || got[0].Version != "4.20.46" || got[0].ServiceIP != "172.30.1.9" {
		t.Errorf("%v %+v", err, got)
	}
	w.kube.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if _, err := List(context.Background(), w.env(), ns); err == nil || !strings.Contains(err.Error(), "listing vSNO Services") {
		t.Errorf("err = %v", err)
	}
}

func TestListWithoutBuilder(t *testing.T) {
	w := newWorld(t, vm("ready", "Running"))
	if _, err := w.kube.CoreV1().Services(ns).Create(context.Background(), svc("ready", "172.30.1.1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	w.runner.errs["probe.sh"] = errBoom
	got, err := List(context.Background(), w.env(), ns)
	if err != nil || got[0].State != Unknown || !strings.Contains(got[0].Detail, "probing through the builder: boom") {
		t.Errorf("%v %+v", err, got)
	}
}

func TestListErrors(t *testing.T) {
	w := newWorld(t)
	w.dyn.PrependReactor("list", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if _, err := List(context.Background(), w.env(), ns); err == nil || !strings.Contains(err.Error(), "listing vSNOs: boom") {
		t.Errorf("err = %v", err)
	}
	empty := newWorld(t)
	if got, err := List(context.Background(), empty.env(), ns); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
}

func TestProbesNothingToDo(t *testing.T) {
	if p, err := Probes(context.Background(), nil, map[string]string{"a": "1.2.3.4"}); p != nil || err != nil {
		t.Errorf("no builder: %v %v", p, err)
	}
	if p, err := Probes(context.Background(), &fakeRunner{}, nil); p != nil || err != nil {
		t.Errorf("no targets: %v %v", p, err)
	}
}

func TestNodeCapacity(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	pod := func(name, phase string, gi string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "x"},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(gi)}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodPhase(phase)}}
	}
	for _, p := range []*corev1.Pod{pod("vm", "Running", "33Gi"), pod("done", "Succeeded", "40Gi")} {
		if _, err := w.kube.CoreV1().Pods("x").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := NodeCapacity(ctx, w.kube, node)
	if err != nil || c.FreeMemoryGiB != 57 || c.Cores != 104 || c.FitDefault != 3 { // 57 GiB / (16 + 1)
		t.Errorf("capacity = %+v, %v", c, err)
	}
	if _, err := NodeCapacity(ctx, w.kube, "nope"); err == nil {
		t.Error("want an error for a missing node")
	}
	w.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if _, err := NodeCapacity(ctx, w.kube, node); err == nil || !strings.Contains(err.Error(), "listing pods") {
		t.Errorf("err = %v", err)
	}
}

func TestGiB(t *testing.T) {
	for in, want := range map[string]int64{"32Gi": 32, "200Gi": 200, "1Ti": 1024, "": 0, "nonsense": 0} {
		if got := gib(in); got != want {
			t.Errorf("gib(%q) = %d", in, got)
		}
	}
	if tail("") != "" || tail("a\nb\n") != ": b" {
		t.Error("tail")
	}
	if API("x") != "https://api.x.vsno.lab:6443" {
		t.Error("API")
	}
}
