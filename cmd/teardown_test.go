package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/teardown"
)

func TestRenderTeardown(t *testing.T) {
	res := &teardown.Result{Namespace: "vsno", Blockers: []string{"VMs outside vsno still exist: other/web"},
		Targets: []teardown.Target{{Kind: "Namespace", Name: "vsno", Detail: "2 VM(s)", Outcome: teardown.Planned},
			{Kind: "LVMCluster", Namespace: "openshift-storage", Name: "vsno-lvmcluster", Outcome: teardown.Deleted}}}
	var b bytes.Buffer
	if err := renderTeardown(&b, res, errors.New("refusing")); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"✗ VMs outside vsno", "planned  Namespace vsno", "2 VM(s)", "deleted  LVMCluster openshift-storage/vsno-lvmcluster"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTeardownNothing(t *testing.T) {
	var b bytes.Buffer
	_ = renderTeardown(&b, &teardown.Result{Namespace: "vsno"}, nil)
	if !strings.Contains(b.String(), "Nothing to delete: namespace vsno does not exist") {
		t.Errorf("out = %q", b.String())
	}
	b.Reset()
	_ = renderTeardown(&b, &teardown.Result{Namespace: "vsno", Blockers: []string{"b"}}, errors.New("refusing"))
	if strings.Contains(b.String(), "Nothing to delete") {
		t.Errorf("a refused run printed nothing-to-delete: %q", b.String())
	}
}

func TestRenderTeardownJSON(t *testing.T) {
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = "table" })
	var b bytes.Buffer
	res := &teardown.Result{Namespace: "vsno", Host: true, Targets: []teardown.Target{{Kind: "Namespace", Name: "vsno", Outcome: teardown.Deleted}}}
	if err := renderTeardown(&b, res, nil); err != nil {
		t.Fatal(err)
	}
	var got teardown.Result
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || !got.Host || got.Targets[0].Outcome != teardown.Deleted {
		t.Fatalf("json = %s (%v)", b.String(), err)
	}
}
