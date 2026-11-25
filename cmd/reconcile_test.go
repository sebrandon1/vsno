package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/reconcile"
)

func TestRenderReconcile(t *testing.T) {
	t.Cleanup(func() { outputFormat = formatTable })
	var b bytes.Buffer
	c := reconcileCmd
	c.SetOut(&b)
	t.Cleanup(func() { c.SetOut(nil) })

	// A healthy environment prints nothing: a cron log stays empty.
	if err := renderReconcile(c, &reconcile.Report{Items: []reconcile.Item{}}); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 0 {
		t.Errorf("healthy printed %q", b.String())
	}

	rep := &reconcile.Report{Namespace: namespace, Pruned: true, Items: []reconcile.Item{
		{Kind: "abandoned vSNO", Name: "old", Action: reconcile.Delete, Done: true,
			Result: precheck.Result{Group: "reconcile", Check: "abandoned vSNO", Status: precheck.Info, Detail: "removed"}},
		{Kind: "cache mirror", Name: "4.20.46", Action: reconcile.Observe,
			Result: precheck.Result{Group: "reconcile", Check: "cache mirror", Status: precheck.Info, Detail: "a mirror is still running"}},
	}}
	b.Reset()
	if err := renderReconcile(c, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"KIND", "abandoned vSNO", "old", "removed", "deleted", "cache mirror", "4.20.46"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q:\n%s", want, b.String())
		}
	}

	b.Reset()
	outputFormat = formatJSON
	if err := renderReconcile(c, &reconcile.Report{Items: []reconcile.Item{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"items": []`) {
		t.Errorf("json = %q, want an empty items array", b.String())
	}
}

func TestReconcileRefusesPruneAndDryRunTogether(t *testing.T) {
	t.Cleanup(func() { rcPrune, rcDryRun = false, false })
	rcPrune, rcDryRun = true, true
	err := runReconcile(reconcileCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--prune and --dry-run") {
		t.Errorf("err = %v", err)
	}
}
