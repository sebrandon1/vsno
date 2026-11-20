package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// densityRunner is a runner whose HyperConverged has overcommit 120% and no
// KSM, and which records each density apply.
func densityRunner(o Options, confirm func(string, string) bool) (*runner, *[]string) {
	s := newSim()
	hc := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"higherWorkloadDensity": map[string]any{"memoryOvercommitPercentage": int64(120)}}}}
	hc.SetAPIVersion("hco.kubevirt.io/v1beta1")
	hc.SetKind("HyperConverged")
	hc.SetNamespace(cluster.CNVNamespace)
	hc.SetName(cluster.HyperConvergedName)
	_ = s.dyn.Tracker().Add(hc)
	var applied []string
	s.dyn.PrependReactor("patch", "hyperconvergeds", func(a k8stesting.Action) (bool, runtime.Object, error) {
		applied = append(applied, string(a.(k8stesting.PatchAction).GetPatch()))
		return true, hc, nil
	})
	return &runner{Env: Env{Dyn: s.dyn, Confirm: confirm}, o: o, cfg: manifests.Config{Node: "worker-0"}, st: &state.State{}}, &applied
}

func TestDensity(t *testing.T) {
	ctx := context.Background()
	yes := func(string, string) bool { return true }
	no := func(string, string) bool { return false }
	for _, c := range []struct {
		name    string
		o       Options
		confirm func(string, string) bool
		detail  string
		err     string
		applied bool
	}{
		{name: "not asked", o: Options{}, confirm: no},
		{name: "already so", o: Options{MemoryOvercommit: 120}, confirm: no, detail: "density unchanged"},
		{name: "dry run", o: Options{MemoryOvercommit: 150, KSM: true, DryRun: true}, confirm: no,
			detail: "would set memory overcommit 120% → 150%, KSM off → on worker-0"},
		{name: "not confirmed", o: Options{KSM: true}, confirm: no, err: "not confirmed"},
		{name: "confirmed", o: Options{MemoryOvercommit: 150}, confirm: yes, detail: "set memory overcommit 120% → 150%", applied: true},
		{name: "--yes", o: Options{KSM: true, Yes: true}, confirm: no, detail: "set KSM off → on worker-0", applied: true},
	} {
		r, applied := densityRunner(c.o, c.confirm)
		detail, err := r.density(ctx)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
		case c.err == "" && (err != nil || detail != c.detail):
			t.Errorf("%s: detail %q, %v; want %q", c.name, detail, err, c.detail)
		case (len(*applied) > 0) != c.applied:
			t.Errorf("%s: applied %q", c.name, *applied)
		}
		// What's recorded is what was there, for teardown to put back.
		if c.applied && (r.st.PreviousDensity == nil || *r.st.PreviousDensity != (state.Density{OvercommitPercent: 120})) {
			t.Errorf("%s: previous = %+v", c.name, r.st.PreviousDensity)
		}
		if !c.applied && r.st.PreviousDensity != nil {
			t.Errorf("%s: recorded %+v without changing anything", c.name, r.st.PreviousDensity)
		}
	}
}

func TestKSMWith(t *testing.T) {
	for _, c := range []struct{ cur, want string }{
		{"", "worker-0"},
		{"worker-0", "worker-0"},
		{state.KSMEveryNode, state.KSMEveryNode}, // never narrowed
		{"worker-1", state.KSMEveryNode},         // never turned off elsewhere
	} {
		if got := ksmWith(c.cur, "worker-0"); got != c.want {
			t.Errorf("ksmWith(%q) = %q, want %q", c.cur, got, c.want)
		}
	}
}
