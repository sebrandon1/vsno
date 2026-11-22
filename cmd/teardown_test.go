package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/state"
	"github.com/sebrandon1/vsno/lib/teardown"
)

// tdResult is a namespace-only plan: two steps kept, two planned.
func tdResult() *teardown.Result {
	obj := func(kind, ns, name string, a apply.Action, step string) teardown.Object {
		return teardown.Object{Change: apply.Change{Kind: kind, Namespace: ns, Name: name, Action: a}, Step: step}
	}
	return &teardown.Result{Namespace: "vsno",
		Steps: []teardown.Step{
			{Name: "support", Outcome: teardown.Planned, Detail: "1 to delete, 1 absent"},
			{Name: "namespace", Outcome: teardown.Planned, Detail: "1 to delete"},
			{Name: "cnv", Outcome: teardown.Kept, Detail: "pass --host to remove"},
		},
		Objects: []teardown.Object{
			obj("Deployment", "vsno", "registry", apply.Delete, "support"),
			obj("Secret", "vsno", "registry-ca", apply.Absent, "support"),
			obj("Namespace", "", "vsno", apply.Delete, "namespace"),
		},
		Unmanaged: []string{"VM seed", "PVC seed-root"},
		Targets:   []teardown.Target{{Kind: "Namespace", Name: "vsno", Outcome: teardown.Planned}},
	}
}

func TestRenderTeardown(t *testing.T) {
	res := tdResult()
	res.Blockers = []string{"VMs outside vsno still exist: other/web"}
	var b bytes.Buffer
	if err := renderTeardown(&b, res, errors.New("refusing"), false, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"✗  VMs outside vsno still exist: other/web\n",
		"~  support    planned  1 to delete, 1 absent\n~  namespace  planned  1 to delete\n=  cnv        kept     pass --host to remove\n",
		"Objects\n  delete  Deployment  vsno/registry\n  absent  Secret      vsno/registry-ca\n  delete  Namespace   vsno\n",
		"3 object(s): 2 to delete, 1 absent. Nothing was changed.",
		"! Deleting namespace vsno also deletes what setup didn't create (2): VM seed, PVC seed-root.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTeardownDryRunAndDone(t *testing.T) {
	tdDryRun = true
	var b bytes.Buffer
	res := tdResult()
	res.Unmanaged = nil
	_ = renderTeardown(&b, res, nil, false, plain)
	tdDryRun = false
	if out := b.String(); !strings.Contains(out, "Dry run: nothing was changed.") || strings.Contains(out, "setup didn't create") {
		t.Errorf("dry run:\n%s", out)
	}

	// After a real run: the steps and a count, not the object list.
	res = tdResult()
	res.Steps[0] = teardown.Step{Name: "support", Outcome: teardown.Deleted, Detail: "1 deleted"}
	res.Objects[0].Action = apply.Removed
	b.Reset()
	_ = renderTeardown(&b, res, nil, true, plain)
	out := b.String()
	for _, want := range []string{"✓  support    deleted  1 deleted", "1 object(s) deleted, and everything else in namespace vsno."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Objects") {
		t.Errorf("a finished run lists no objects:\n%s", out)
	}
}

func TestRenderTeardownNothing(t *testing.T) {
	var b bytes.Buffer
	_ = renderTeardown(&b, &teardown.Result{Namespace: "vsno"}, nil, false, plain)
	if !strings.Contains(b.String(), "Nothing to delete: namespace vsno does not exist") {
		t.Errorf("out = %q", b.String())
	}
	b.Reset()
	_ = renderTeardown(&b, &teardown.Result{Namespace: "vsno", Blockers: []string{"b"}}, errors.New("refusing"), false, plain)
	if strings.Contains(b.String(), "Nothing to delete") {
		t.Errorf("a refused run printed nothing-to-delete: %q", b.String())
	}
}

func TestRenderTeardownJSON(t *testing.T) {
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = "table" })
	var b bytes.Buffer
	res := &teardown.Result{Namespace: "vsno", Host: true, Targets: []teardown.Target{{Kind: "Namespace", Name: "vsno", Outcome: teardown.Deleted}}}
	if err := renderTeardown(&b, res, nil, false, plain); err != nil {
		t.Fatal(err)
	}
	var got teardown.Result
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || !got.Host || got.Targets[0].Outcome != teardown.Deleted {
		t.Fatalf("json = %s (%v)", b.String(), err)
	}
}

func TestRenderTeardownForced(t *testing.T) {
	var b bytes.Buffer
	res := tdResult()
	res.Blockers, res.Forced = []string{"VMs outside vsno still exist: other/web"}, true
	_ = renderTeardown(&b, res, nil, false, plain)
	if out := b.String(); !strings.Contains(out, "!  VMs outside vsno still exist: other/web (ignored: --force)") || strings.Contains(out, "✗") {
		t.Errorf("out = %q", out)
	}
}

func TestRenderTeardownPlanShown(t *testing.T) {
	var b bytes.Buffer
	_ = renderTeardown(&b, tdResult(), errors.New("not confirmed"), true, plain)
	if b.Len() != 0 {
		t.Errorf("not confirmed: the plan is already on screen, got:\n%s", b.String())
	}
	res := tdResult()
	res.Objects[0].Action = apply.Removed // failed partway
	_ = renderTeardown(&b, res, errors.New("x"), true, plain)
	if !strings.Contains(b.String(), "1 object(s) deleted") {
		t.Errorf("failed partway: show what was deleted, got:\n%s", b.String())
	}
}

// Without -n, teardown names the environments it won't pick between instead
// of silently deleting one (#54).
func TestTeardownRefusesSeveralEnvironments(t *testing.T) {
	for _, envs := range [][]string{nil, {"vsno"}} {
		if err := severalEnvironments(envs); err != nil {
			t.Errorf("%v: %v", envs, err)
		}
	}
	err := severalEnvironments([]string{"vsno", "vsno-dev"})
	if err == nil || !strings.Contains(err.Error(), "several vsno environments (vsno, vsno-dev); pick one with -n") {
		t.Errorf("several: %v", err)
	}
	if got := otherEnvironments([]string{"vsno", "vsno-dev"}, "vsno-dev"); len(got) != 1 || got[0] != "vsno" {
		t.Errorf("others = %v", got)
	}
	if got := otherEnvironments([]string{"vsno"}, "vsno"); len(got) != 0 {
		t.Errorf("others of the only one = %v", got)
	}
}

// A plain teardown says it puts HyperConverged's density back.
func TestRenderTeardownRestoresDensity(t *testing.T) {
	res := tdResult()
	res.RestoreDensity = &state.Density{OvercommitPercent: 120}
	var b bytes.Buffer
	_ = renderTeardown(&b, res, nil, false, plain)
	if want := "HyperConverged goes back to memory overcommit 120%, KSM off, as before vsno setup changed it."; !strings.Contains(b.String(), want) {
		t.Errorf("missing %q:\n%s", want, b.String())
	}
}
