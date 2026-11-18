package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/precheck"
)

func TestRenderReportEligible(t *testing.T) {
	r := &precheck.Report{
		Results: []precheck.Result{
			{Check: "Cluster access", Status: precheck.Pass, Detail: "cluster-admin"},
			{Check: "vm-worker: cpu", Status: precheck.Fail, Detail: "4 threads; one VM needs 16", Hint: "use a bigger node"},
			{Check: "Seed registry", Status: precheck.Info, Detail: "skipped", Hint: "info hints are hidden"},
		},
		Suggested: precheck.Suggested{Node: "bm-worker", Disk: "/dev/disk/by-id/wwn-0x1", DiskSizeGB: 1919,
			HostDNS: "172.30.0.10", LVMSChannel: "stable-4.22", CNVChannel: "stable"},
	}
	var b bytes.Buffer
	renderReport(&b, r)
	out := b.String()
	for _, want := range []string{
		"✓  Cluster access", "✗  vm-worker: cpu", "→ use a bigger node",
		"node          bm-worker", "/dev/disk/by-id/wwn-0x1 (1919 GB)", "Note: 1 other check(s) failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "info hints are hidden") {
		t.Error("hints on info rows should not be printed")
	}
}

func TestRenderReportIneligible(t *testing.T) {
	r := &precheck.Report{Results: []precheck.Result{{Check: "Eligible node", Status: precheck.Fail, Detail: "none"}}}
	var b bytes.Buffer
	renderReport(&b, r)
	if !strings.Contains(b.String(), "cannot host vSNOs yet (1 failing check(s))") {
		t.Fatalf("unexpected output:\n%s", b.String())
	}
}

func TestInvalidOutputFormat(t *testing.T) {
	outputFormat = "yaml"
	defer func() { outputFormat = "table" }()
	if err := rootCmd.PersistentPreRunE(rootCmd, nil); err == nil {
		t.Fatal("want error for --output yaml")
	}
}
