package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	"github.com/sebrandon1/vsno/lib/status"
)

func statusReport() *status.Report {
	r := &status.Report{
		Namespace: "vsno", Exists: true, Managed: true, Node: "worker-1",
		Host:     []status.Component{{Name: "cnv", Status: precheck.Pass, Detail: "kubevirt-hyperconverged-operator.v1 Succeeded"}},
		Support:  []status.Component{{Name: "registry", Status: precheck.Warn, Detail: "0/1 ready"}},
		Registry: "172.30.0.50:5000",
		VMs:      []status.VM{{Name: "seed", Status: "Running", Ready: true, Node: "worker-1"}},
		VSNOs: []guest.Guest{{Name: "b", State: guest.Finishing, Version: "4.22.0-0.nightly-2026-11-10-072952", Detail: "3 operators progressing"},
			{Name: "c", State: guest.Importing, Requested: "4.20", Detail: "40%"}},
	}
	for i := range 7 {
		r.Stages = append(r.Stages, state.Stage{Name: "setup", Status: state.Done, Time: time.Date(2026, 11, 18, i, 0, 0, 0, time.UTC), Detail: fmt.Sprintf("run %d", i)})
	}
	return r
}

func TestRenderStatusTable(t *testing.T) {
	var b bytes.Buffer
	if err := renderStatus(&b, statusReport(), plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Environment vsno on node worker-1", "✓  cnv", "!  registry", "registry 172.30.0.50:5000",
		"seed  Running  true   worker-1", "run 6", "run 2",
		"vSNOs\n  NAME  STATE          VERSION                             DETAIL\n  b     finishing      4.22.0-0.nightly-2026-11-10-072952  3 operators progressing\n  c     importing-iso  4.20                                40%\n",
		"Other VMs\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "run 1") {
		t.Errorf("shows more than %d stages:\n%s", statusStages, out)
	}
}

func TestRenderStatusHeadings(t *testing.T) {
	cases := map[string]*status.Report{
		"does not exist; run `vsno setup -n x`": {Namespace: "x"},
		"was not created by vsno":               {Namespace: "x", Exists: true},
		"Environment x\n":                       {Namespace: "x", Exists: true, Managed: true},
	}
	for want, r := range cases {
		var b bytes.Buffer
		if err := renderStatus(&b, r, plain); err != nil || !strings.Contains(b.String(), want) {
			t.Errorf("want %q in:\n%s", want, b.String())
		}
		if !r.Exists && strings.Contains(b.String(), "Support services") {
			t.Error("support section for a missing namespace")
		}
	}
}

func TestRenderStatusJSON(t *testing.T) {
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = "table" })
	var b bytes.Buffer
	if err := renderStatus(&b, statusReport(), plain); err != nil {
		t.Fatal(err)
	}
	var got status.Report
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || len(got.Stages) != 7 || got.VMs[0].Node != "worker-1" {
		t.Fatalf("json = %s (%v)", b.String(), err)
	}
}
