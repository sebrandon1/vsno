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
			{Check: "Cluster access", Status: precheck.Pass, Detail: "cluster-admin", Group: precheck.GroupCluster},
			{Check: "Seed registry", Status: precheck.Info, Detail: "in-cluster", Hint: "info hints are hidden", Group: precheck.GroupInstall},
			{Check: "Worker nodes", Status: precheck.Info, Detail: "3 candidate(s)", Group: precheck.GroupWorkers},
			{Node: "bm-worker", Check: "cpu", Status: precheck.Pass, Detail: "104 threads", Group: precheck.GroupWorkers},
			{Node: "bm-2", Check: "cpu", Status: precheck.Pass, Detail: "64 threads", Group: precheck.GroupWorkers},
			{Node: "vm-worker", Check: "virtualization", Status: precheck.Warn, Detail: "nested KVM", Hint: "prefer bare metal", Group: precheck.GroupWorkers},
			{Node: "vm-worker", Check: "cpu", Status: precheck.Info, Detail: "not usable: 4 threads; one VM needs 16", Group: precheck.GroupWorkers},
			{Check: "Eligible node", Status: precheck.Pass, Detail: "bm-worker (of 2 eligible)", Group: precheck.GroupWorkers},
			{Check: "Network overlap", Status: precheck.Fail, Detail: "overlap", Group: precheck.GroupCluster},
		},
		Nodes: []precheck.NodeSummary{{Name: "bm-worker", Eligible: true}, {Name: "bm-2", Eligible: true}, {Name: "vm-worker"}},
		Suggested: precheck.Suggested{Node: "bm-worker", Disk: "/dev/disk/by-id/wwn-0x1", DiskSizeGB: 1919,
			HostDNS: "172.30.0.10", LVMSChannel: "stable-4.22", CNVChannel: "stable"},
	}
	var b bytes.Buffer
	renderReport(&b, r, plain)
	out := b.String()
	want := []string{
		"Cluster\n✓  Cluster access   cluster-admin\n✗  Network overlap  overlap\n",
		"\nOperators, storage and registry\ni  Seed registry  in-cluster\n",
		"\nWorkers\ni  Worker nodes  3 candidate(s)\n",
		"✓  bm-worker  eligible, suggested\n   ✓  cpu  104 threads\n",
		"✓  bm-2  eligible\n",
		"i  vm-worker  not usable\n   !  virtualization  nested KVM\n                      → prefer bare metal\n   i  cpu             4 threads; one VM needs 16\n",
		"✓  Eligible node  bm-worker (of 2 eligible)\n",
		"node          bm-worker", "/dev/disk/by-id/wwn-0x1 (1919 GB)", "Note: 1 other check(s) failed",
	}
	last := -1
	for _, w := range want { // in this order
		i := strings.Index(out, w)
		if i < 0 {
			t.Errorf("output missing %q:\n%s", w, out)
			continue
		}
		if i < last {
			t.Errorf("%q is out of order:\n%s", w, out)
		}
		last = i
	}
	if strings.Contains(out, "info hints are hidden") {
		t.Error("hints on info rows should not be printed")
	}
}

func TestNodeVerdict(t *testing.T) {
	rows := func(st ...precheck.Status) []precheck.Result {
		var r []precheck.Result
		for _, s := range st {
			r = append(r, precheck.Result{Node: "n1", Status: s})
		}
		return r
	}
	evaluated := []precheck.NodeSummary{{Name: "n1"}}
	for _, c := range []struct {
		name      string
		r         precheck.Report
		block     []precheck.Result
		icon, say string
	}{
		{"not usable, another node chosen", precheck.Report{Nodes: evaluated, Suggested: precheck.Suggested{Node: "n2"}}, rows(precheck.Info), "i", "not usable"},
		{"not usable, none chosen", precheck.Report{Nodes: evaluated}, rows(precheck.Fail), "✗", "not usable"},
		{"probe failed", precheck.Report{}, rows(precheck.Warn), "!", "not evaluated"},
		{"probe failed, worst wins", precheck.Report{}, rows(precheck.Fail, precheck.Warn), "✗", "not evaluated"},
		{"probe failed, info only", precheck.Report{}, rows(precheck.Info), "i", "not evaluated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if icon, say := nodeVerdict(c.block, c.r.Nodes, c.r.Suggested.Node); icon != c.icon || say != c.say {
				t.Errorf("got %s %q, want %s %q", icon, say, c.icon, c.say)
			}
		})
	}
}

func TestRenderReportIneligible(t *testing.T) {
	r := &precheck.Report{Results: []precheck.Result{
		{Node: "vm-worker", Check: "cpu", Status: precheck.Fail, Detail: "4 threads", Group: precheck.GroupWorkers},
		{Check: "Eligible node", Status: precheck.Fail, Detail: "none", Group: precheck.GroupWorkers},
	}, Nodes: []precheck.NodeSummary{{Name: "vm-worker"}}}
	var b bytes.Buffer
	renderReport(&b, r, plain)
	out := b.String()
	for _, w := range []string{"Workers\n✗  vm-worker  not usable\n   ✗  cpu  4 threads\n", "cannot host vSNOs yet (2 failing check(s))"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	if strings.HasPrefix(out, "\n") || strings.Contains(out, "Cluster") {
		t.Errorf("empty sections must be skipped:\n%s", out)
	}
}

func TestRenderReportNoProbe(t *testing.T) {
	r := &precheck.Report{Results: []precheck.Result{{Check: "Hardware probe", Status: precheck.Warn, Detail: "skipped", Group: precheck.GroupWorkers}}}
	var b bytes.Buffer
	renderReport(&b, r, plain)
	if out := b.String(); !strings.Contains(out, "No node suggested: the hardware probe was skipped") || strings.Contains(out, "cannot host") {
		t.Errorf("out = %q", out)
	}
}

func TestInvalidOutputFormat(t *testing.T) {
	outputFormat = "yaml"
	defer func() { outputFormat = "table" }()
	if err := rootCmd.PersistentPreRunE(rootCmd, nil); err == nil {
		t.Fatal("want error for --output yaml")
	}
}
