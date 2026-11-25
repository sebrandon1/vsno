package guest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// existing is a world with vSNO seed fully created.
func existing(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, vm("seed", "Running"), dv("seed-root", "Succeeded", ""), dv("seed-iso", "Succeeded", "100.0%"), svcObj())
	if err := SetDNS(context.Background(), w.env(), ns, "seed", true); err != nil {
		t.Fatal(err)
	}
	return w
}

// svcObj is the vSNO's Service in the dynamic fake, where Delete reads it.
func svcObj() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "seed", "namespace": ns}}}
}

func unstructuredString(obj map[string]any, path ...string) (string, bool, error) {
	return unstructured.NestedString(obj, path...)
}

func TestDeletePlanAndDryRun(t *testing.T) {
	w := existing(t)
	res, err := Delete(context.Background(), w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range res.Objects {
		got = append(got, c.Kind+" "+c.Name+" "+string(c.Action))
	}
	want := "VirtualMachine seed delete,DataVolume seed-root delete,DataVolume seed-iso delete,PersistentVolumeClaim seed-root absent,Service seed delete,DNS entry seed.conf delete,Builder files /work/guests/seed delete"
	if strings.Join(got, ",") != want {
		t.Errorf("plan:\n%s\nwant:\n%s", strings.Join(got, ","), want)
	}
	if _, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "seed", metav1.GetOptions{}); err != nil {
		t.Error("a dry run deleted the VM")
	}
}

func TestDelete(t *testing.T) {
	w := existing(t)
	asked := ""
	shown := 0
	res, err := Delete(context.Background(), w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"},
		Confirm:  func(q, a string) bool { asked = a; return true },
		ShowPlan: func(*DeleteResult) { shown++ }})
	if err != nil {
		t.Fatal(err)
	}
	if asked != "seed" || shown != 1 {
		t.Errorf("confirm: asked %q, shown %d", asked, shown)
	}
	if n := apply.Count(res.Objects, apply.Delete); n != 0 {
		t.Errorf("%d parts still marked for deletion: %+v", n, res.Objects)
	}
	if n := apply.Count(res.Objects, apply.Removed); n != 6 {
		t.Errorf("%d parts removed, want 6 (the absent PVC is not one): %+v", n, res.Objects)
	}
	if _, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "seed", metav1.GetOptions{}); err == nil {
		t.Error("VM still there")
	}
	if w.dnsKeys(t)["seed.conf"] != "" {
		t.Error("DNS entry still there")
	}
	if rm := w.runner.ran("rm.sh"); len(rm) != 1 || rm[0].args[0] != "seed" {
		t.Errorf("rm.sh = %+v", rm)
	}
}

// A vSNO restored from a snapshot keeps its root disk in a PVC where an
// install has a root DataVolume. Delete must remove the PVC, or the disk leaks
// until the whole namespace is torn down.
func TestDeleteRestoredRootPVC(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, vm("seed", "Running"), svcObj())
	if _, err := w.kube.CoreV1().PersistentVolumeClaims(ns).Create(ctx,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "seed-root"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := SetDNS(ctx, w.env(), ns, "seed", true); err != nil {
		t.Fatal(err)
	}
	res, err := Delete(ctx, w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"}, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	for _, c := range res.Objects {
		if c.Kind == KindPVC && c.Name == "seed-root" && c.Action == apply.Removed {
			removed = true
		}
	}
	if !removed {
		t.Errorf("the root PVC was not deleted: %+v", res.Objects)
	}
	if _, err := w.kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "seed-root", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("root PVC still there: %v", err)
	}
}

func TestDeleteRefusals(t *testing.T) {
	w := existing(t)
	ctx := context.Background()
	if _, err := Delete(ctx, w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"}}); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Errorf("no Confirm: %v", err)
	}
	if _, err := Delete(ctx, w.env(), DeleteOptions{Namespace: ns, Names: []string{"ghost"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "no vSNO named ghost") {
		t.Errorf("missing: %v", err)
	}
	if _, err := Delete(ctx, w.env(), DeleteOptions{Namespace: ns, Names: []string{"Bad"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Errorf("bad name: %v", err)
	}
	w.dyn.PrependReactor("get", "datavolumes", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if _, err := Delete(ctx, w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "reading DataVolume seed-root") {
		t.Errorf("read error: %v", err)
	}
}

func TestDeleteLeftoversOfAFailedCreate(t *testing.T) {
	// Only the DNS entry is left: delete cleans it up instead of refusing.
	w := newWorld(t)
	if err := SetDNS(context.Background(), w.env(), ns, "half", true); err != nil {
		t.Fatal(err)
	}
	res, err := Delete(context.Background(), w.env(), DeleteOptions{Namespace: ns, Names: []string{"half"}, Yes: true})
	if err != nil || apply.Count(res.Objects, apply.Removed) != 2 || w.dnsKeys(t)["half.conf"] != "" {
		t.Errorf("%v %+v", err, res.Objects)
	}
}

func TestDeleteFailures(t *testing.T) {
	w := existing(t)
	w.runner.errs["rm.sh"] = errBoom
	if _, err := Delete(context.Background(), w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "deleting Builder files") {
		t.Errorf("rm error: %v", err)
	}
	w = existing(t)
	w.dyn.PrependReactor("delete", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if _, err := Delete(context.Background(), w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "deleting VirtualMachine seed") {
		t.Errorf("delete error: %v", err)
	}
	w = existing(t)
	w.dyn.PrependReactor("delete", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, nil }) // never goes away
	if _, err := Delete(context.Background(), w.env(), DeleteOptions{Namespace: ns, Names: []string{"seed"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "waiting for vSNO seed to be deleted") {
		t.Errorf("wait error: %v", err)
	}
}

func TestSetRunningAndWait(t *testing.T) {
	w := existing(t)
	ctx := context.Background()
	if _, err := w.kube.CoreV1().Services(ns).Create(ctx, svc("seed", "172.30.9.9"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e := w.env()
	if err := SetRunning(ctx, e, ns, "seed", false); err != nil {
		t.Fatal(err)
	}
	u, _ := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "seed", metav1.GetOptions{})
	if s, _, _ := unstructuredString(u.Object, "spec", "runStrategy"); s != "Halted" {
		t.Errorf("runStrategy = %q", s)
	}
	if err := SetRunning(ctx, e, ns, "seed", true); err != nil {
		t.Fatal(err)
	}
	u, _ = w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "seed", metav1.GetOptions{})
	if s, _, _ := unstructuredString(u.Object, "spec", "runStrategy"); s != "Always" {
		t.Errorf("runStrategy = %q", s)
	}
	if err := SetRunning(ctx, e, ns, "ghost", true); err == nil {
		t.Error("want an error for a missing vSNO")
	}

	w.runner.outputs["probe.sh"] = probeLine("seed", `"cv":{"version":"4.20.46","available":true}`)
	if st, err := WaitState(ctx, e, ns, "seed", Ready); err != nil || st != Ready {
		t.Errorf("WaitState = %s, %v", st, err)
	}
	e.Timeout = 1
	w.runner.outputs["probe.sh"] = ""
	if _, err := WaitState(ctx, e, ns, "seed", Ready); err == nil {
		t.Error("want a timeout")
	}
}

// A wait rides out a few failed reads of the vSNO (the host API, or the
// network to it, blinking), and gives up when they keep failing.
func TestWaitStateRidesOutFailedReads(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		fails int
		ok    bool
	}{{maxPollErrors, true}, {maxPollErrors + 1, false}} {
		w := existing(t)
		e := w.env()
		e.Timeout = time.Minute
		// As in TestSetRunningAndWait, so it reads Ready.
		if _, err := w.kube.CoreV1().Services(ns).Create(ctx, svc("seed", "172.30.9.9"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := SetRunning(ctx, e, ns, "seed", true); err != nil {
			t.Fatal(err)
		}
		w.runner.outputs["probe.sh"] = probeLine("seed", `"cv":{"version":"4.20.46","available":true}`)
		n := 0
		w.dyn.PrependReactor("get", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
			if n++; n <= c.fails {
				return true, nil, errors.New("read: no route to host")
			}
			return false, nil, nil
		})
		st, err := WaitState(ctx, e, ns, "seed", Ready)
		if c.ok && (err != nil || st != Ready) {
			t.Errorf("%d failed reads: %s, %v", c.fails, st, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "no route to host")) {
			t.Errorf("%d failed reads: want the read error, got %v", c.fails, err)
		}
	}
}

func TestAccess(t *testing.T) {
	w := existing(t)
	ctx := context.Background()
	if _, err := w.kube.CoreV1().Services(ns).Create(ctx, svc("seed", "172.30.9.9"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e := w.env()
	if err := Oc(ctx, e, ns, "seed", []string{"get", "nodes"}, builder.IO{}); err != nil {
		t.Fatal(err)
	}
	if err := SSH(ctx, e, ns, "seed", []string{"uptime"}, builder.IO{}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.runner.outputs["inline"] = "apiVersion: v1\n"
	if err := Kubeconfig(ctx, e, ns, "seed", &out, nil); err != nil || out.String() != "apiVersion: v1\n" {
		t.Errorf("kubeconfig: %q %v", out.String(), err)
	}
	calls := w.runner.ran("inline")
	if len(calls) != 3 || strings.Join(calls[0].args, " ") != "seed 172.30.9.9 vsno.lab get nodes" ||
		strings.Join(calls[1].args, " ") != "seed 172.30.9.9 vsno.lab uptime" || strings.Join(calls[2].args, " ") != "seed" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestAccessErrors(t *testing.T) {
	w := existing(t)
	ctx := context.Background()
	e := w.env()
	if _, err := Screenshot(ctx, e, ns, "seed"); err == nil {
		t.Error("the fake clientset has no REST client: want an error")
	}
	var out strings.Builder
	for _, c := range []struct {
		name string
		fn   func() error
		want string
	}{
		{"missing", func() error { return Oc(ctx, e, ns, "ghost", nil, builder.IO{}) }, "no vSNO named ghost"},
		{"bad name", func() error { return SSH(ctx, e, ns, "Bad", nil, builder.IO{}) }, "invalid name"},
		{"kubeconfig of missing", func() error { return Kubeconfig(ctx, e, ns, "ghost", &out, nil) }, "no vSNO named ghost"},
		{"screenshot of missing", func() error { _, err := Screenshot(ctx, e, ns, "ghost"); return err }, "no vSNO named ghost"},
	} {
		if err := c.fn(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// No Service yet, or one without an IP.
	w2 := newWorld(t, vm("seed", "Running"))
	if err := Oc(ctx, w2.env(), ns, "seed", nil, builder.IO{}); err == nil || !strings.Contains(err.Error(), "reading the Service") {
		t.Errorf("no service: %v", err)
	}
	if _, err := w2.kube.CoreV1().Services(ns).Create(ctx, svc("seed", corev1.ClusterIPNone), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Oc(ctx, w2.env(), ns, "seed", nil, builder.IO{}); err == nil || !strings.Contains(err.Error(), "has no ClusterIP") {
		t.Errorf("headless: %v", err)
	}
}

// tolerate lets maxPollErrors failed reads in a row through, fails the next,
// and starts over after a good read.
func TestReadErrorsTolerate(t *testing.T) {
	boom := errors.New("no route to host")
	var r readErrors
	for i := range maxPollErrors {
		if err := r.tolerate(boom); err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
	}
	if err := r.tolerate(boom); !errors.Is(err, boom) {
		t.Errorf("read %d: %v, want the error", maxPollErrors+1, err)
	}
	if err := r.tolerate(nil); err != nil || r != 0 {
		t.Errorf("a good read: %v, count %d", err, r)
	}
	if err := r.tolerate(boom); err != nil {
		t.Errorf("after a good read: %v", err)
	}
}

// A vSNO with no VM yet is still building its ISO while the Service create
// added first is young; older than any build, it is abandoned.
func TestMissingState(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	env.Now = func() time.Time { return now }
	ctx := context.Background()

	if st, _ := MissingState(ctx, env, ns, "gone"); st != Unknown {
		t.Errorf("nothing at all = %s, want unknown", st)
	}
	for name, age := range map[string]time.Duration{"fresh": time.Minute, "old": 2 * abandonGrace} {
		s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
			Labels: map[string]string{LabelName: name}, CreationTimestamp: metav1.Time{Time: now.Add(-age)}}}
		if _, err := w.kube.CoreV1().Services(ns).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := MissingState(ctx, env, ns, "fresh"); st != Building {
		t.Errorf("fresh Service = %s, want building-iso", st)
	}
	if st, d := MissingState(ctx, env, ns, "old"); st != Abandoned || !strings.Contains(d, "vsno delete") {
		t.Errorf("old Service = %s %q, want abandoned with the delete hint", st, d)
	}
}
