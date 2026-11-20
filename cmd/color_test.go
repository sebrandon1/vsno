package cmd

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/status"
	"github.com/sebrandon1/vsno/lib/teardown"
	"github.com/sebrandon1/vsno/lib/ui"
)

var (
	plain   = ui.Palette{}
	colored = ui.Palette{On: true}
)

// TestColorKeepsLayout renders each command both ways: without the color
// codes, the colored output must be identical to the plain one, so columns
// still line up.
func TestColorKeepsLayout(t *testing.T) {
	report := &precheck.Report{
		Results: []precheck.Result{
			{Check: "Cluster access", Status: precheck.Pass, Detail: "cluster-admin", Group: precheck.GroupCluster},
			{Check: "Hub-managed cluster", Status: precheck.Warn, Detail: "klusterlet", Hint: "check policies", Group: precheck.GroupCluster},
			{Node: "bm-worker", Check: "cpu", Status: precheck.Pass, Detail: "104 threads", Group: precheck.GroupWorkers},
			{Node: "bm-worker", Check: "memory", Status: precheck.Info, Detail: "held", Group: precheck.GroupWorkers},
			{Node: "vm-worker", Check: "virtualization", Status: precheck.Warn, Detail: "nested", Hint: "use bare metal", Group: precheck.GroupWorkers},
		},
		Nodes:     []precheck.NodeSummary{{Name: "bm-worker", Eligible: true}, {Name: "vm-worker"}},
		Suggested: precheck.Suggested{Node: "bm-worker"},
	}
	targets := tdResult()
	targets.Blockers, targets.Forced = []string{"VMs elsewhere"}, true
	done := tdResult()
	done.Steps[0].Outcome, done.Objects[0].Action = teardown.Deleted, apply.Removed
	stuck := statusReport()
	stuck.VMs = append(stuck.VMs, status.VM{Name: "target", Status: "Provisioning", Node: "worker-1"})
	stuck.Stages[len(stuck.Stages)-1].Status = "failed"
	stuck.Stages[len(stuck.Stages)-2].Status = "running"

	for name, render := range map[string]func(*bytes.Buffer, ui.Palette){
		"precheck": func(b *bytes.Buffer, p ui.Palette) { renderReport(b, report, p) },
		"precheck failing": func(b *bytes.Buffer, p ui.Palette) {
			renderReport(b, &precheck.Report{Results: []precheck.Result{{Check: "Eligible node", Status: precheck.Fail, Group: precheck.GroupWorkers}}}, p)
		},
		"status":        func(b *bytes.Buffer, p ui.Palette) { _ = renderStatus(b, stuck, p) },
		"setup":         func(b *bytes.Buffer, p ui.Palette) { _ = renderSetup(b, setupResult(), false, p) },
		"setup dry run": func(b *bytes.Buffer, p ui.Palette) { _ = renderSetup(b, dryRunResult(), true, p) },
		"teardown":      func(b *bytes.Buffer, p ui.Palette) { _ = renderTeardown(b, targets, errors.New("x"), false, p) },
		"list wide": func(b *bytes.Buffer, p ui.Palette) {
			old := outputFormat
			outputFormat = formatWide
			defer func() { outputFormat = old }()
			_ = renderList(b, testListing(), time.Now(), p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var p, c bytes.Buffer
			render(&p, plain)
			render(&c, colored)
			if !strings.Contains(c.String(), "\033[") {
				t.Fatalf("colored output has no color:\n%s", c.String())
			}
			if strings.Contains(p.String(), "\033[") {
				t.Fatalf("plain output has color codes:\n%q", p.String())
			}
			if got := ui.StripANSI(c.String()); got != p.String() {
				t.Errorf("layout differs with color.\nplain:\n%s\ncolored, stripped:\n%s", p.String(), got)
			}
		})
	}
}

func TestPaletteFollowsOutput(t *testing.T) {
	if palette(&bytes.Buffer{}).On {
		t.Error("a buffer is not a terminal")
	}
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = "table" })
	if palette(os.Stdout).On {
		t.Error("JSON output is never colored")
	}
}

func TestNoColorFlagSetsNOCOLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	noColor = true
	t.Cleanup(func() { noColor = false })
	if err := rootCmd.PersistentPreRunE(rootCmd, nil); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("NO_COLOR") == "" {
		t.Error("--no-color must set NO_COLOR so the progress on stderr is plain too")
	}
}
