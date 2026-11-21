package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/setup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSetupSettings(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "vsno.yaml")
	if err := os.WriteFile(cfg, []byte("node: worker-1\ndisk: /dev/disk/by-id/wwn-0x5000000000000002\ndiskSizeGB: 1000\nhostDNS: 172.30.0.10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("nodee: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, config, node, disk string
		want                     precheck.Suggested
		wantErr                  string
	}{
		{name: "config only", config: cfg, want: precheck.Suggested{Node: "worker-1", Disk: "/dev/disk/by-id/wwn-0x5000000000000002", DiskSizeGB: 1000, HostDNS: "172.30.0.10"}},
		{name: "--node drops the config disk", config: cfg, node: "worker-2", want: precheck.Suggested{Node: "worker-2", HostDNS: "172.30.0.10"}},
		{name: "--node and --disk", config: cfg, node: "worker-2", disk: "/dev/disk/by-id/wwn-0x5000000000000003",
			want: precheck.Suggested{Node: "worker-2", Disk: "/dev/disk/by-id/wwn-0x5000000000000003", HostDNS: "172.30.0.10"}},
		{name: "same --node keeps the disk", config: cfg, node: "worker-1", want: precheck.Suggested{Node: "worker-1", Disk: "/dev/disk/by-id/wwn-0x5000000000000002", DiskSizeGB: 1000, HostDNS: "172.30.0.10"}},
		{name: "no config", node: "worker-3", want: precheck.Suggested{Node: "worker-3"}},
		{name: "unknown key", config: bad, wantErr: "parsing"},
		{name: "missing file", config: filepath.Join(dir, "nope.yaml"), wantErr: "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			suConfig, suNode, suDisk = c.config, c.node, c.disk
			t.Cleanup(func() { suConfig, suNode, suDisk = "", "", "" })
			got, err := setupSettings()
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %+v %v, want %+v", got, err, c.want)
			}
		})
	}
}

func TestConfirm(t *testing.T) {
	cases := map[string]bool{"worker-1\n": true, "  worker-1  \n": true, "yes\n": false, "": false}
	for in, want := range cases {
		var w bytes.Buffer
		if got := confirm(strings.NewReader(in), &w, "Erase disk?", "worker-1"); got != want {
			t.Errorf("input %q: got %v", in, got)
		}
		if w.String() != "Erase disk? [worker-1]: " {
			t.Errorf("prompt = %q", w.String())
		}
	}
}

func TestConfirmOnTerminalRefusesPipes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	var out bytes.Buffer
	if confirmOnTerminal(r, &out)("Erase?", "worker-1") {
		t.Error("approved without a terminal")
	}
	if !strings.Contains(out.String(), "no terminal") {
		t.Errorf("out = %q", out.String())
	}
}

func setupResult() *setup.Result {
	return &setup.Result{
		Namespace: "vsno",
		Settings:  precheck.Suggested{Node: "worker-1"},
		Warnings:  []string{"worker-1: memory: 20 GiB schedulable"},
		Steps: []setup.Step{
			{Name: "cnv", Outcome: setup.Reused, Detail: "kubevirt-hyperconverged-operator.v1.0.0"},
			{Name: "support", Outcome: setup.Applied, Detail: "1 created, 0 updated, 1 unchanged"},
		},
		Changes:  []apply.Change{{Kind: "Service", Name: "s3", Action: apply.Created}, {Kind: "Service", Name: "registry", Action: apply.Unchanged}},
		Registry: "172.30.0.50:5000",
	}
}

func TestRenderSetupTable(t *testing.T) {
	var b bytes.Buffer
	if err := renderSetup(&b, setupResult(), false, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"! worker-1: memory", "=  cnv", "reused", "✓  support", "node worker-1, 2 object(s): 1 created, 0 updated, 1 unchanged",
		"registry 172.30.0.50:5000 (credentials in secret vsno/registry-auth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dry run") {
		t.Error("not a dry run")
	}
}

func dryRunResult() *setup.Result {
	res := setupResult()
	res.Registry = ""
	res.Steps = []setup.Step{{Name: "support", Outcome: setup.Planned, Detail: "1 to create, 1 to take over"}}
	res.Changes = []apply.Change{
		{Kind: "Service", Namespace: "vsno", Name: "s3", Action: apply.Create},
		{Kind: "Deployment", Namespace: "vsno", Name: "builder", Action: apply.TakeOver},
		{Kind: "Namespace", Name: "vsno", Action: apply.Reapply},
	}
	res.Objects = []*unstructured.Unstructured{{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "vsno"}}}}
	return res
}

func TestRenderSetupDryRun(t *testing.T) {
	var b bytes.Buffer
	if err := renderSetup(&b, dryRunResult(), true, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"~  support  planned  1 to create, 1 to take over\n",
		"Objects\n  create     Service     vsno/s3\n  take over  Deployment  vsno/builder\n  reapply    Namespace   vsno\n",
		"node worker-1, 3 object(s): 1 to create, 1 to reapply, 1 to take over. Dry run: nothing was changed.",
		"! 1 object(s) exist but were not created by vsno; setup would overwrite them.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "apiVersion") {
		t.Error("manifests are printed only with --manifests")
	}

	// No take-overs, no warning.
	res := dryRunResult()
	res.Changes = res.Changes[:1]
	b.Reset()
	_ = renderSetup(&b, res, true, plain)
	if strings.Contains(b.String(), "were not created by vsno") {
		t.Errorf("warning without take-overs:\n%s", b.String())
	}
}

func TestRenderSetupManifests(t *testing.T) {
	suManifests = true
	t.Cleanup(func() { suManifests = false })
	var b bytes.Buffer
	if err := renderSetup(&b, dryRunResult(), true, plain); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.HasPrefix(out, "---\napiVersion: v1\nkind: Namespace\n") || !strings.Contains(out, "Objects") {
		t.Errorf("--manifests output:\n%s", out)
	}
}

func TestPlanAction(t *testing.T) {
	for a, want := range map[apply.Action]string{
		apply.Create: colored.Green("create"), apply.Reapply: colored.Dim("reapply"), apply.TakeOver: colored.Yellow("take over"),
		apply.Delete: colored.Red("delete"), apply.Absent: colored.Dim("absent"), apply.Removed: colored.Green("deleted"),
	} {
		if got := planAction(colored, a); got != want {
			t.Errorf("%s = %q, want %q", a, got, want)
		}
	}
}

func TestRenderSetupOperatorUpgrade(t *testing.T) {
	res := setupResult()
	res.Steps[0] = setup.Step{Name: "cnv", Outcome: setup.Upgraded, Detail: "v1.0.0 → v1.1.0"}
	var b bytes.Buffer
	if err := renderSetup(&b, res, false, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"✓", "cnv", "upgraded", "v1.0.0 → v1.1.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// A dry run plans the upgrade rather than reporting one.
	res.Steps[0] = setup.Step{Name: "cnv", Outcome: setup.Planned, Detail: "upgrade v1.0.0 → v1.1.0"}
	b.Reset()
	if err := renderSetup(&b, res, true, plain); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "~") || !strings.Contains(out, "planned") || !strings.Contains(out, "upgrade v1.0.0 → v1.1.0") {
		t.Errorf("planned upgrade missing:\n%s", out)
	}
}

func TestRenderSetupJSONAndEmpty(t *testing.T) {
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = "table" })
	var b bytes.Buffer
	if err := renderSetup(&b, setupResult(), false, plain); err != nil {
		t.Fatal(err)
	}
	var got setup.Result
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || got.Registry != "172.30.0.50:5000" || len(got.Steps) != 2 {
		t.Fatalf("json = %s (%v)", b.String(), err)
	}

	outputFormat = "table"
	b.Reset()
	if err := renderSetup(&b, nil, false, plain); err != nil || b.Len() != 0 {
		t.Errorf("nil result: %q %v", b.String(), err)
	}
	b.Reset()
	if err := renderSetup(&b, &setup.Result{Warnings: []string{"w"}}, false, plain); err != nil || strings.Contains(b.String(), "object(s)") {
		t.Errorf("failed-before-steps result: %q", b.String())
	}
}

func TestOperatorChannels(t *testing.T) {
	got, err := operatorChannels([]string{"cnv=candidate", "lvms=stable-4.22"})
	if err != nil {
		t.Fatal(err)
	}
	if got["cnv"] != "candidate" || got["lvms"] != "stable-4.22" {
		t.Errorf("channels = %v", got)
	}
	if m, err := operatorChannels(nil); err != nil || len(m) != 0 {
		t.Errorf("no channels = %v %v", m, err)
	}
	for _, bad := range []string{"cnv", "cnv=", "=candidate", "virt=candidate"} {
		if _, err := operatorChannels([]string{bad}); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestCheckOvercommit(t *testing.T) {
	for _, c := range []struct {
		pct int
		ok  bool
	}{{0, true}, {100, true}, {150, true}, {1000, true}, {99, false}, {1001, false}, {-5, false}} {
		if err := checkOvercommit(c.pct); (err == nil) != c.ok {
			t.Errorf("%d: %v", c.pct, err)
		}
	}
}
