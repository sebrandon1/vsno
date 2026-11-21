package run

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/state"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const ns = "vsno-test"

// listing answers registry.sh list with out.
type listing struct{ out string }

func (l listing) Run(_ context.Context, _ string, args []string, s builder.IO) error {
	if len(args) > 1 && args[1] == "list" {
		_, _ = io.WriteString(s.Stdout, l.out)
	}
	return nil
}

func testEnv(hostVersion string, stable ...string) Env {
	cv := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"desired": map[string]any{"version": hostVersion}}}}
	cv.SetAPIVersion("config.openshift.io/v1")
	cv.SetKind("ClusterVersion")
	cv.SetName("version")
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{precheck.GVRClusterVersion: "ClusterVersionList"}, cv)
	kube := kfake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"}, Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte("CA")}},
	)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	return Env{
		Env: seed.Env{Env: guest.Env{Kube: kube, Dyn: dyn, Builder: listing{}, Poll: time.Millisecond,
			Now: func() time.Time { now = now.Add(time.Second); return now }}},
		Stable: func(_ context.Context, minor string) ([]string, error) {
			if minor != "4.22" {
				return nil, errors.New("unexpected minor " + minor)
			}
			return stable, nil
		},
	}
}

func TestPlanDefaults(t *testing.T) {
	e := testEnv("4.22.0-0.nightly-2026-11-03", "4.22.1", "4.22.2", "4.22.3")
	o := Options{Namespace: ns}
	o.defaults()
	p, err := makePlan(context.Background(), e, o)
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetVersion != "4.22.2" || p.SeedVersion != "4.22.3" || p.Target != "target" || p.Seed != "seed" {
		t.Errorf("plan = %+v", p)
	}
	if want := []Stage{StageSetup, StageVSNOs, StageSeed, StageUpgrade}; !slices.Equal(p.Stages, want) {
		t.Errorf("stages = %v, want %v", p.Stages, want)
	}
	for _, want := range []string{"seed     vSNO seed at 4.22.3", "target   vSNO target at 4.22.2", "each vSNO's catalog", "leave the target in UpgradeCompleted"} {
		if !strings.Contains(p.String(), want) {
			t.Errorf("plan text lacks %q:\n%s", want, p)
		}
	}
}

func TestPlanChoices(t *testing.T) {
	ctx := context.Background()
	e := testEnv("4.22.0", "4.22.1", "4.22.2")
	for _, c := range []struct {
		name    string
		o       Options
		stages  []Stage
		wantErr string
	}{
		{"a seed image needs no seed vSNO", Options{SeedImage: "quay.io/me/seed:4.22.2", SeedVersion: "4.22.2", TargetVersion: "4.22.1"},
			[]Stage{StageSetup, StageVSNOs, StageUpgrade}, ""},
		{"delete afterwards", Options{TargetVersion: "4.22.1", SeedVersion: "4.22.2", Delete: true},
			[]Stage{StageSetup, StageVSNOs, StageSeed, StageUpgrade, StageDelete}, ""},
		{"target not older", Options{TargetVersion: "4.22.2", SeedVersion: "4.22.1"}, nil, "must be older than the seed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := c.o
			o.Namespace = ns
			o.defaults()
			p, err := makePlan(ctx, e, o)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(p.Stages, c.stages) {
				t.Errorf("stages = %v, err = %v", p.Stages, err)
			}
		})
	}
	if _, err := makePlan(ctx, testEnv("4.22.0", "4.22.1"), Options{Namespace: ns}); err == nil || !strings.Contains(err.Error(), "needs two") {
		t.Errorf("one release: err = %v", err)
	}
}

func TestOptionsCheck(t *testing.T) {
	for _, c := range []struct {
		o    Options
		want string
	}{
		{Options{Finalize: true, Rollback: true}, "opposites"},
		{Options{Delete: true, Finalize: true}, "would be undone"},
		{Options{TargetName: "x", SeedName: "x"}, "name them apart"},
		{Options{TargetName: "x", SeedName: "x", SeedImage: "img"}, ""},
	} {
		o := c.o
		o.defaults()
		err := o.check()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: err = %v, want %q", c.o, err, c.want)
		}
	}
}

func TestMatches(t *testing.T) {
	for _, c := range []struct {
		g       guest.Guest
		version string
		ok      bool
	}{
		{guest.Guest{Name: "t", Version: "4.22.1"}, "4.22.1", true},
		{guest.Guest{Name: "t", Version: "4.22.1"}, "4.22", true},
		{guest.Guest{Name: "t", Requested: "4.22-nightly", Version: "4.22.0-0.nightly-x"}, "4.22-nightly", true},
		{guest.Guest{Name: "t"}, "4.22.1", true}, // its version isn't known yet
		{guest.Guest{Name: "t", Version: "4.21.9"}, "4.22.1", false},
	} {
		if err := matches(c.g, c.version); (err == nil) != c.ok {
			t.Errorf("%+v vs %s: err = %v", c.g, c.version, err)
		}
	}
}

// A seed image an earlier run made is used again instead of a new generation.
func TestEnsureSeedReusesTheImage(t *testing.T) {
	e := testEnv("4.22.0")
	e.Builder = listing{out: `{"repo":"seeds/seed","tag":"4.22.2","digest":"sha256:a","created":"2026-11-19T23:00:00Z","labels":{},"size":1}` + "\n"}
	p := &Plan{Seed: "seed", SeedVersion: "4.22.2"}
	if err := ensureSeed(context.Background(), e, Options{Namespace: ns}, p); err != nil {
		t.Fatal(err)
	}
	if p.SeedImage != "registry.vsno-test.svc.cluster.local:5000/seeds/seed:4.22.2" {
		t.Errorf("seed image = %q", p.SeedImage)
	}
}

// Stages a run finished are recorded, so the next plan says it resumes.
func TestRecordAndResume(t *testing.T) {
	ctx := context.Background()
	e := testEnv("4.22.0", "4.22.1", "4.22.2")
	if err := state.Save(ctx, e.Kube, ns, &state.State{}); err != nil {
		t.Fatal(err)
	}
	record(ctx, e, ns, StageSetup, nil)
	record(ctx, e, ns, StageVSNOs, errors.New("boom"))
	if got := finished(ctx, e, ns); !slices.Equal(got, []Stage{StageSetup}) {
		t.Errorf("finished = %v", got)
	}
	o := Options{Namespace: ns}
	o.defaults()
	p, err := makePlan(ctx, e, o)
	if err != nil || !strings.Contains(p.String(), "resuming: an earlier run finished [setup]") {
		t.Errorf("plan = %s, err = %v", p, err)
	}
}

func TestRunNeedsConfirmation(t *testing.T) {
	e := testEnv("4.22.0", "4.22.1", "4.22.2")
	ran := false
	e.Setup = func(context.Context) error { ran = true; return nil }
	var asked string
	e.Confirm = func(q, _ string) bool { asked = q; return false }
	res, err := Run(context.Background(), e, Options{Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || ran || res == nil || !strings.Contains(asked, "Plan for vsno-test") {
		t.Errorf("err = %v, ran = %v, asked = %q", err, ran, asked)
	}
}

// A failing stage stops the run, names the stage, and is recorded as failed.
func TestRunStopsAtAFailedStage(t *testing.T) {
	ctx := context.Background()
	e := testEnv("4.22.0", "4.22.1", "4.22.2")
	if err := state.Save(ctx, e.Kube, ns, &state.State{}); err != nil {
		t.Fatal(err)
	}
	e.Setup = func(context.Context) error { return errors.New("no bare-metal worker") }
	res, err := Run(ctx, e, Options{Namespace: ns, Yes: true})
	if err == nil || !strings.HasPrefix(err.Error(), "setup: no bare-metal worker") || res.Timings[StageSetup] == "" || res.Timings[StageVSNOs] != "" {
		t.Errorf("err = %v, timings = %v", err, res.Timings)
	}
	st, _, _ := state.Load(ctx, e.Kube, ns)
	if last, ok := st.Last("run:setup"); !ok || last.Status != state.Failed {
		t.Errorf("recorded = %+v %v", last, ok)
	}
}

// withNode adds the environment's node with allocatable memory, and the
// resources guest.List and the density check read.
func withNode(t *testing.T, e Env, memory string) Env {
	t.Helper()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse(memory), corev1.ResourceCPU: resource.MustParse("104")}}}
	kube := kfake.NewClientset(n)
	if err := state.Save(context.Background(), kube, ns, &state.State{Node: "worker-0"}); err != nil {
		t.Fatal(err)
	}
	e.Kube = kube
	e.Dyn = dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cluster.GVRVirtualMachine: "VirtualMachineList", guest.GVRDataVolume: "DataVolumeList", cluster.GVRVMI: "VirtualMachineInstanceList",
	})
	e.Defaults(ns) // as Run does
	return e
}

// Both vSNOs are created at once when the node has room for both; otherwise
// the seed alone, and the upgrade stage creates the target later.
func TestToCreate(t *testing.T) {
	ctx := context.Background()
	p := &Plan{Seed: "seed", SeedVersion: "4.22.2", Target: "target", TargetVersion: "4.22.1"}
	for _, c := range []struct {
		memory string
		want   []string
	}{
		{"90Gi", []string{"seed", "target"}}, // 2 × 17 GiB fit
		{"30Gi", []string{"seed"}},
	} {
		o := Options{Namespace: ns}
		o.defaults()
		got, err := toCreate(ctx, withNode(t, testEnv("4.22.0"), c.memory), o, p)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v %v, want %v", c.memory, got, err, c.want)
		}
	}
	// The seed exists (an earlier run): the target is created beside it,
	// and its own create checks the room.
	e := withNode(t, testEnv("4.22.0"), "30Gi")
	vm := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"runStrategy": "Always"}}}
	vm.SetAPIVersion("kubevirt.io/v1")
	vm.SetKind("VirtualMachine")
	vm.SetNamespace(ns)
	vm.SetName("seed")
	vm.SetLabels(map[string]string{guest.LabelName: "seed"})
	if _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Create(ctx, vm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	o := Options{Namespace: ns}
	o.defaults()
	if got, err := toCreate(ctx, e, o, p); err != nil || !slices.Equal(got, []string{"seed", "target"}) {
		t.Errorf("seed exists: %v %v", got, err)
	}
	// With a seed image there is only the target.
	if got, err := toCreate(ctx, withNode(t, testEnv("4.22.0"), "30Gi"), Options{Namespace: ns}, &Plan{Target: "target"}); err != nil || !slices.Equal(got, []string{"target"}) {
		t.Errorf("seed image: %v %v", got, err)
	}
}

// Sizes left out are vsno create's defaults: guest.Create takes them as given.
func TestOptionsDefaults(t *testing.T) {
	o := Options{}
	o.defaults()
	if o.Cores != guest.DefaultCores || o.MemoryGiB != guest.DefaultMemoryGiB || o.SeedName != "seed" || o.TargetName != "target" {
		t.Errorf("%+v", o)
	}
	o = Options{Cores: 8, MemoryGiB: 24}
	o.defaults()
	if o.Cores != 8 || o.MemoryGiB != 24 {
		t.Errorf("given sizes changed: %+v", o)
	}
}
