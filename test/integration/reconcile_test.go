package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// reconcileReport is enough of the JSON to assert on.
type reconcileReport struct {
	Namespace string `json:"namespace"`
	Pruned    bool   `json:"pruned"`
	Items     []struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Action string `json:"action"`
		Done   bool   `json:"done"`
		Error  string `json:"error"`
	} `json:"items"`
}

// A sweep from cron has no terminal, so it must produce parseable JSON, never
// ask a question, and exit 0 when it merely found something.
func TestReconcileJSON(t *testing.T) {
	h := newFakeHost(t)
	got := run(t, h.kubeconfig(t), "-o", "json", "reconcile")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", got.code, got.stderr)
	}
	var rep reconcileReport
	if err := json.Unmarshal([]byte(got.stdout), &rep); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, got.stdout)
	}
	if rep.Namespace != "vsno" || rep.Pruned {
		t.Errorf("report = %+v", rep)
	}
	if rep.Items == nil {
		t.Errorf("items is null, want an empty array: %s", got.stdout)
	}
}

// --prune has no --yes and no prompt: it must not block or mention a terminal.
func TestReconcilePruneNeverPrompts(t *testing.T) {
	h := newFakeHost(t)
	got := run(t, h.kubeconfig(t), "reconcile", "--prune")
	if strings.Contains(got.stderr, "no terminal") || strings.Contains(got.stdout, "no terminal") {
		t.Errorf("asked for a terminal:\n%s%s", got.stdout, got.stderr)
	}
	if got.code != 0 {
		t.Errorf("exit %d, stderr:\n%s", got.code, got.stderr)
	}
}

// --prune with --dry-run is a usage error, refused before touching a cluster.
func TestReconcileRefusesPruneWithDryRun(t *testing.T) {
	h := newFakeHost(t)
	got := run(t, h.kubeconfig(t), "reconcile", "--prune", "--dry-run")
	if got.code == 0 {
		t.Fatalf("exit 0, stdout:\n%s", got.stdout)
	}
	if len(h.requests()) != 0 {
		t.Errorf("touched the cluster first: %v", h.requests())
	}
}

// A namespace that cannot be read is a failure, not a finding: the cron exit
// code must mean "the sweep did not run".
func TestReconcileMissingNamespace(t *testing.T) {
	h := newFakeHost(t)
	got := run(t, h.kubeconfig(t), "-n", "absent", "reconcile", "-o", "json")
	if got.code == 0 {
		t.Errorf("exit 0 for a missing namespace; stdout:\n%s", got.stdout)
	}
}
