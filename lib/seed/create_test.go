package seed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCreate(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[sgCheck] = []resp{{Out: ""}} // nothing there yet
	fa.OcReplies[sgGet] = []resp{{Out: ""}}   // and the poller's own read
	fa.OcReplies[sgPoll] = []resp{{Out: sgRunningJS}, {Out: sgDoneJS}}
	// The generation ends with its SeedGenerator still there, completed.
	fa.OcReplies[sgCheck] = []resp{{Out: ""}, {Out: sgDoneJS}}
	e := world(t, fa)
	res, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantImage := Ref(testRegistry, "a", "4.20.46")
	if res.Image != wantImage || res.Version != "4.20.46" || res.LCA != "lifecycle-agent.v4.20.7" {
		t.Errorf("result = %+v", res)
	}
	if !fa.Ran("oc apply") || !strings.Contains(strings.Join(fa.Stdin, "\n"), wantImage) {
		t.Errorf("the SeedGenerator was not applied with the image: %q", fa.Stdin)
	}
	if !fa.Ran("oc delete seedgenerators seedimage") || !fa.Ran("oc delete secret seedgen") {
		t.Errorf("finish left the SeedGenerator behind: %q", fa.Calls)
	}
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := vm.GetAnnotations()[AnnSeedImage]; got != wantImage {
		t.Errorf("recorded seed = %q, want %q", got, wantImage)
	}
	if v, ok := vm.GetAnnotations()[guest.AnnSeeding]; ok {
		t.Errorf("the seeding mark stayed: %q", v)
	}
}

// A generation that fails leaves the vSNO usable: the mark is cleared, and the
// error says what LCA said.
func TestCreateClearsTheMarkWhenGenerationFails(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[sgGet] = []resp{{Out: ""}}
	fa.OcReplies[sgPoll] = []resp{{Out: sgFailedJS}}
	fa.OcReplies[sgCheck] = []resp{{Out: sgFailedJS}}
	e := world(t, fa)
	res, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a"})
	if err == nil || !strings.Contains(err.Error(), "push denied") {
		t.Fatalf("err = %v, want the generation failure", err)
	}
	if res.Image == "" {
		t.Errorf("result = %+v, want the image it tried", res)
	}
	vm, gerr := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if v, ok := vm.GetAnnotations()[guest.AnnSeeding]; ok {
		t.Errorf("the seeding mark stayed: %q", v)
	}
}

// A second generation is refused in one read, before the precheck spends
// minutes on a vSNO LCA is already using.
func TestCreateRefusesASecondGeneration(t *testing.T) {
	fa := readyAccess()
	fa.OcReplies[sgCheck] = []resp{{Out: sgRunningJS}}
	e := world(t, fa)
	_, err := Create(context.Background(), e, CreateOptions{Namespace: ns, Name: "a"})
	if err == nil || !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), "Launching imager container") {
		t.Fatalf("err = %v, want the generation it names", err)
	}
	if len(fa.Calls) != 1 {
		t.Errorf("it did more than the one read: %q", fa.Calls)
	}
}

// While LCA has the guest's services stopped its API doesn't answer, so the
// mark vsno left is the only evidence a generation is running.
func TestCreateRefusesOnTheMarkWhileTheAPIIsDown(t *testing.T) {
	fa := readyAccess()
	fa.OcReplies[sgCheck] = []resp{{Err: errors.New("connection refused")}}
	e := world(t, fa)
	markGenerating(t, e, e.Now().Add(-5*time.Minute))
	_, err := Create(context.Background(), e, CreateOptions{Namespace: ns, Name: "a"})
	if err == nil || !strings.Contains(err.Error(), "started 5m") || !strings.Contains(err.Error(), "LCA is still generating") {
		t.Fatalf("err = %v, want the mark's age", err)
	}
}

// Ctrl-C leaves LCA generating on the node: the mark has to survive until the
// SeedGenerator says the generation ended, or list calls the silence a fault.
func TestCreateKeepsTheMarkWhenInterrupted(t *testing.T) {
	fa := readyAccess()
	fa.OcReplies[sgGet] = []resp{{Out: ""}}
	fa.OcReplies[sgPoll] = []resp{{Out: sgRunningJS}}
	fa.OcReplies[sgCheck] = []resp{{Err: errors.New("connection refused")}, {Err: errors.New("connection refused")}}
	e := world(t, fa)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Progress = interruptProgress{cancel: cancel}
	if _, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a"}); err == nil {
		t.Fatal("an interrupted generation must fail")
	}
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := vm.GetAnnotations()[guest.AnnSeeding]; !ok {
		t.Error("the seeding mark was cleared while LCA is still generating")
	} else if _, ok := guest.SeedingSince(v, e.Now()); !ok {
		t.Errorf("the mark left is not one list trusts: %q", v)
	}
}

// markGenerating writes the seeding mark on the world's vSNO.
func markGenerating(t *testing.T, e Env, at time.Time) {
	t.Helper()
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ann := vm.GetAnnotations()
	ann[guest.AnnSeeding] = at.UTC().Format(time.RFC3339)
	vm.SetAnnotations(ann)
	if _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Update(context.Background(), vm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// interruptProgress cancels the run the first time the seed generation reports
// progress, standing in for Ctrl-C at that moment.
type interruptProgress struct{ cancel context.CancelFunc }

func (p interruptProgress) Start(title string) precheck.ProgressStep {
	return &interruptStep{cancel: p.cancel, armed: title == "Generating the seed image"}
}

type interruptStep struct {
	cancel context.CancelFunc
	armed  bool
}

func (s *interruptStep) Update(string) {
	if s.armed {
		s.armed = false
		s.cancel()
	}
}
func (s *interruptStep) SetProgress(int, int) {}
func (s *interruptStep) Done(string)          {}
func (s *interruptStep) Warn(string)          {}
func (s *interruptStep) Fail(string)          {}

// The early check reads LCA's SeedGenerator, and falls back to the mark only
// when there is nothing else to read. A finished generation clears a stale
// mark; one past list's window is left alone.
func TestCheckSeedGen(t *testing.T) {
	for _, c := range []struct {
		name    string
		reply   resp
		mark    time.Duration // how long ago it was made; 0 = none
		want    string        // "" = allowed
		present bool          // the mark afterwards
	}{
		{"running", resp{Out: sgRunningJS}, 0, "already running on the vSNO: Launching imager container", false},
		{"finished", resp{Out: sgDoneJS}, 10 * time.Minute, "", false},
		{"nothing and a recent mark", resp{Out: ""}, 5 * time.Minute, "started 5m", true},
		{"nothing and no mark", resp{Out: ""}, 0, "", false},
		{"API down and a recent mark", resp{Err: errors.New("connection refused")}, 3 * time.Minute, "started 3m", true},
		{"API down and a stale mark", resp{Err: errors.New("connection refused")}, 2 * time.Hour, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fa := readyAccess()
			fa.OcReplies[sgCheck] = []resp{c.reply}
			e := world(t, fa)
			if c.mark > 0 {
				markGenerating(t, e, e.Now().Add(-c.mark))
			}
			err := checkSeedGen(context.Background(), e, ns, "a")
			if c.want == "" && err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "a", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, has := vm.GetAnnotations()[guest.AnnSeeding]; has != c.present {
				t.Errorf("mark present = %v, want %v", has, c.present)
			}
		})
	}
}

// RunningSeedGen answers "is a generation under way?" the same way the early
// check does: LCA's SeedGenerator, or the mark while its API is down.
func TestRunningSeedGen(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply resp
		mark  time.Duration
		want  string // String(), or "" for nil
	}{
		{"running", resp{Out: sgRunningJS}, 0, "Launching imager container"},
		{"finished", resp{Out: sgDoneJS}, time.Hour, ""},
		{"no generator, marked", resp{Out: ""}, 4 * time.Minute, "started 4m"},
		{"no generator, no mark", resp{Out: ""}, 0, ""},
		{"API down, marked", resp{Err: errors.New("connection refused")}, 3 * time.Minute, "started 3m"},
		{"API down, stale mark", resp{Err: errors.New("connection refused")}, 2 * time.Hour, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fa := readyAccess()
			fa.OcReplies[sgCheck] = []resp{c.reply}
			e := world(t, fa)
			if c.mark > 0 {
				markGenerating(t, e, e.Now().Add(-c.mark))
			}
			g := RunningSeedGen(context.Background(), e, ns, "a")
			if c.want == "" && g != nil {
				t.Fatalf("got %v, want nil", g)
			}
			// A mark is written whole-second, so its age is a prefix, not exact.
			if c.want != "" && (g == nil || !strings.HasPrefix(g.String(), c.want)) {
				t.Fatalf("got %v, want %q", g, c.want)
			}
		})
	}
}

// The node keeps LCA's orchestration directory after an interrupted generation,
// with the services it stopped still off.
func TestSeedGenInterrupted(t *testing.T) {
	const check = "test -d /var/lib/lca/ibu-seedgen-orch && echo orch ; systemctl is-enabled kubelet crio"
	for _, c := range []struct {
		name  string
		reply resp
		want  bool
		err   bool
	}{
		{"interrupted", resp{Out: "orch\ndisabled\ndisabled\n"}, true, false},
		{"running", resp{Out: "orch\nenabled\nenabled\n"}, false, false},
		{"no directory", resp{Out: "enabled\nenabled\n"}, false, false},
		{"no answer", resp{Err: errors.New("connection refused")}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fa := &fakeAccess{SSHReplies: map[string][]resp{check: {c.reply}}}
			got, err := SeedGenInterrupted(context.Background(), fa)
			if got != c.want || (err != nil) != c.err {
				t.Errorf("= %v, %v; want %v, err %v", got, err, c.want, c.err)
			}
		})
	}
}

// Only a SeedGenerator that is gone, completed or failed ends a generation;
// one that can't be read is not evidence either way.
func TestSeedGenEnded(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply resp
		want  bool
	}{
		{"gone", resp{Out: ""}, true},
		{"completed", resp{Out: sgDoneJS}, true},
		{"failed", resp{Out: sgFailedJS}, true},
		{"running", resp{Out: sgRunningJS}, false},
		{"unreachable", resp{Err: errors.New("connection refused")}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fa := &fakeAccess{OcReplies: map[string][]resp{sgCheck: {c.reply}}}
			if got := seedGenEnded(context.Background(), fa); got != c.want {
				t.Errorf("seedGenEnded = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCreateRefusesANotReadyVSNO(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess() // everything but the vSNO's state is fine
	e := world(t, fa)
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(vm.Object, "Stopped", "status", "printableStatus"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Update(ctx, vm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a"}); err == nil || !strings.Contains(err.Error(), "a seed needs a finished install") {
		t.Errorf("err = %v, want the precheck's refusal", err)
	}
	if strings.Contains(strings.Join(fa.Stdin, "\n"), "seedImage") {
		t.Errorf("a seed generation was applied anyway: %q", fa.Stdin)
	}
}

// finish stops the vSNO when asked, and records that it did.
func TestCreateStopsTheVSNO(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[sgGet] = []resp{{Out: ""}}
	fa.OcReplies[sgPoll] = []resp{{Out: sgDoneJS}}
	e := world(t, fa)
	res, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a", Stop: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped {
		t.Errorf("result = %+v, want Stopped", res)
	}
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running"); running {
		t.Errorf("the VM was not stopped: %v", vm.Object["spec"])
	}
}
