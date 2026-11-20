package isos

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const ns = "vsno-test"

// runner answers the listing with listing, and records what it ran.
type runner struct {
	listing string
	err     error
	ran     []string
}

func (r *runner) Run(_ context.Context, s string, args []string, streams builder.IO) error {
	switch {
	case strings.Contains(s, "df --output"):
		r.ran = append(r.ran, "list")
		if r.err != nil {
			return r.err
		}
		_, _ = io.WriteString(streams.Stdout, r.listing)
	case strings.Contains(s, "rm -rf"):
		r.ran = append(r.ran, "rm-vsno "+strings.Join(args, " "))
	case strings.Contains(s, "rm -f"):
		r.ran = append(r.ran, "rm-installer "+strings.Join(args, " "))
	}
	return nil
}

const listing = `{"kind":"volume","used":10737418240,"total":64424509440}
{"kind":"served","name":"a","size":1395864371,"modified":"2026-11-19T10:00:00Z"}
{"kind":"served","name":"gone","size":1395864371,"modified":"2026-11-18T10:00:00Z"}
{"kind":"build","name":"a","size":2147483648}
{"kind":"build","name":"gone","size":1073741824}
{"kind":"installer","name":"openshift-install-aaaa","release":"quay.io/x:4.20.46","version":"4.20.46","size":734003200,"modified":"2026-11-19T10:00:00Z"}
{"kind":"installer","name":"openshift-install-bbbb","release":"quay.io/x:4.19.9","version":"4.19.9","size":734003200,"modified":"2026-11-01T10:00:00Z"}
{"kind":"installer","name":"openshift-install-cccc","release":"quay.io/x:4.18.1","version":"4.18.1","size":734003200,"modified":"2026-10-01T10:00:00Z"}
{"kind":"installer","name":"openshift-install-dddd","version":"4.17.0","size":734003200,"modified":"2026-09-01T10:00:00Z"}
{"kind":"base","name":"base-iso","size":1395864371}
`

var listKinds = map[schema.GroupVersionResource]string{
	cluster.GVRVirtualMachine: "VirtualMachineList",
	guest.GVRDataVolume:       "DataVolumeList",
}

// testEnv is an environment whose only vSNO is a, installed from 4.20.46.
func testEnv(t *testing.T, r *runner) Env {
	t.Helper()
	objs := []runtime.Object{vmObj(), dv("a-iso", "3Gi"), dv("gone-iso", "3Gi"), dv("a-root", "200Gi")}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	return Env{Env: guest.Env{Kube: kfake.NewClientset(), Dyn: dyn, Builder: r}}
}

// vmObj is vSNO a, installed from 4.20.46.
func vmObj() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"namespace": ns, "name": "a", "annotations": map[string]any{guest.AnnImage: "quay.io/x:4.20.46"}},
	}}
}

func dv(name, size string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{"storage": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": size}}}},
	}}
}

func TestList(t *testing.T) {
	rep, err := List(context.Background(), testEnv(t, &runner{listing: listing}), ns)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Used != 10<<30 || rep.Total != 60<<30 {
		t.Errorf("volume = %d/%d", rep.Used, rep.Total)
	}
	if len(rep.Entries) != 9 {
		t.Errorf("%d entries, want 9", len(rep.Entries))
	}
	if len(rep.Disks) != 2 || rep.DiskBytes != 6<<30 {
		t.Fatalf("disks = %+v, %d bytes", rep.Disks, rep.DiskBytes)
	}
}

func TestListUse(t *testing.T) {
	rep, err := List(context.Background(), testEnv(t, &runner{listing: listing}), ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		use  Use
	}{
		{"served a", InUse}, {"build a", InUse}, {"served gone", Orphaned}, {"build gone", Orphaned},
		{"installer openshift-install-aaaa", InUse}, {"installer openshift-install-bbbb", Unused},
		{"installer openshift-install-cccc", Unused}, {"installer openshift-install-dddd", Unknown},
		{"base base-iso", ""},
	} {
		if got := useOf(rep, c.name); got != c.use {
			t.Errorf("%s = %q, want %q", c.name, got, c.use)
		}
	}
	// The installer in use names its vSNO.
	for _, e := range rep.Entries {
		if e.Name == "openshift-install-aaaa" && !slices.Equal(e.Users, []string{"a"}) {
			t.Errorf("installer users = %q", e.Users)
		}
	}
	if rep.Disks[0].Name != "a-iso" || rep.Disks[0].Use != InUse || rep.Disks[1].Use != Orphaned {
		t.Errorf("disks = %+v", rep.Disks)
	}
}

// useOf is the Use of rep's entry "<kind> <name>".
func useOf(rep *Report, key string) Use {
	for _, e := range rep.Entries {
		if string(e.Kind)+" "+e.Name == key {
			return e.Use
		}
	}
	return "missing"
}

func TestListErrors(t *testing.T) {
	if _, err := List(context.Background(), testEnv(t, &runner{err: io.ErrUnexpectedEOF}), ns); err == nil || !strings.Contains(err.Error(), "listing the builder") {
		t.Errorf("got %v", err)
	}
	bad := testEnv(t, &runner{listing: "not json\n"})
	if _, err := List(context.Background(), bad, ns); err == nil || !strings.Contains(err.Error(), "reading the builder's file list") {
		t.Errorf("got %v", err)
	}
}

func TestPlanFor(t *testing.T) {
	rep, err := List(context.Background(), testEnv(t, &runner{listing: listing}), ns)
	if err != nil {
		t.Fatal(err)
	}
	p := planFor(rep, 0)
	if !slices.Equal(p.VSNOs, []string{"gone"}) {
		t.Errorf("vSNOs = %q", p.VSNOs)
	}
	// The unknown installer (no .ref) is left alone; the two unused go.
	if len(p.Installers) != 2 || p.Installers[0].Name != "openshift-install-bbbb" || p.Installers[1].Name != "openshift-install-cccc" {
		t.Errorf("installers = %+v", p.Installers)
	}
	if len(p.Disks) != 1 || p.Disks[0].Name != "gone-iso" {
		t.Errorf("disks = %+v", p.Disks)
	}
	// gone's ISO (1.3Gi) + build dir (1Gi) + two installers (1.4Gi) + its DV (3Gi).
	if want := int64(1395864371 + 1073741824 + 2*734003200 + 3<<30); p.Bytes != want {
		t.Errorf("bytes = %d, want %d", p.Bytes, want)
	}
	// --keep 1 leaves the newest unused installer.
	if p := planFor(rep, 1); len(p.Installers) != 1 || p.Installers[0].Name != "openshift-install-cccc" {
		t.Errorf("keep 1 = %+v", p.Installers)
	}
	// Everything in use: nothing to do.
	clean := &Report{Entries: []Entry{{Kind: Served, Name: "a", Use: InUse}}, Disks: []Disk{{Name: "a-iso", Use: InUse}}}
	if p := planFor(clean, 0); !p.Empty() {
		t.Errorf("clean plan = %+v", p)
	}
}

func TestPrune(t *testing.T) {
	t.Run("dry run changes nothing", testPruneDryRun)
	t.Run("not confirmed", testPruneUnconfirmed)
	t.Run("confirmed", testPruneConfirmed)
	t.Run("nothing to prune", testPruneNothing)
}

func testPruneDryRun(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	res, err := Prune(ctx, testEnv(t, r), PruneOptions{Namespace: ns, DryRun: true})
	if err != nil || len(res.Removed) != 0 || len(r.ran) != 1 || r.ran[0] != "list" {
		t.Errorf("res %+v, ran %q", res, r.ran)
	}
}

func testPruneUnconfirmed(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	shown := 0
	_, err := Prune(ctx, testEnv(t, r), PruneOptions{Namespace: ns,
		ShowPlan: func(p *Plan) { shown = len(p.VSNOs) + len(p.Installers) + len(p.Disks) }, Confirm: func(_, _ string) bool { return false }})
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || shown != 4 || len(r.ran) != 1 {
		t.Errorf("err %v, shown %d, ran %q", err, shown, r.ran)
	}
}

func testPruneConfirmed(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	var asked string
	env := testEnv(t, r)
	res, err := Prune(ctx, env, PruneOptions{Namespace: ns, Confirm: func(_, a string) bool { asked = a; return true }})
	if err != nil || asked != "prune" {
		t.Fatalf("err %v, asked %q", err, asked)
	}
	if len(res.Removed) != 4 || res.Bytes == 0 {
		t.Errorf("removed %q (%d bytes)", res.Removed, res.Bytes)
	}
	if want := []string{"list", "rm-vsno gone", "rm-installer openshift-install-bbbb", "rm-installer openshift-install-cccc"}; !slices.Equal(r.ran, want) {
		t.Errorf("ran %q, want %q", r.ran, want)
	}
	if _, err := env.Dyn.Resource(guest.GVRDataVolume).Namespace(ns).Get(ctx, "gone-iso", metav1.GetOptions{}); err == nil {
		t.Error("the orphaned DataVolume is still there")
	}
	if _, err := env.Dyn.Resource(guest.GVRDataVolume).Namespace(ns).Get(ctx, "a-iso", metav1.GetOptions{}); err != nil {
		t.Errorf("deleted the ISO of a live vSNO: %v", err)
	}
}

func testPruneNothing(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: `{"kind":"served","name":"a","size":10}`}
	env := testEnv(t, r)
	e := Env{Env: env.Env}
	e.Dyn = dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, vmObj(), dv("a-iso", "3Gi"))
	res, err := Prune(ctx, e, PruneOptions{Namespace: ns, Yes: true})
	if err != nil || !res.Plan.Empty() || len(res.Removed) != 0 {
		t.Errorf("res %+v, %v", res, err)
	}
}

func TestBytesText(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{{0, "nothing"}, {512 << 20, "512Mi"}, {3 << 30, "3.0Gi"}} {
		if got := bytesText(c.n); got != c.want {
			t.Errorf("bytesText(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
