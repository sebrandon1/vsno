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

func TestRenderSetupDryRun(t *testing.T) {
	res := setupResult()
	res.Registry = ""
	res.Objects = []*unstructured.Unstructured{{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "vsno"}}}}
	var b bytes.Buffer
	if err := renderSetup(&b, res, true, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "---\napiVersion: v1\nkind: Namespace\n") || !strings.Contains(out, "2 object(s) would be applied; dry run, nothing was changed") {
		t.Errorf("dry run output:\n%s", out)
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
