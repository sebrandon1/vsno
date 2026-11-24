package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/release"
	"github.com/sebrandon1/vsno/lib/snapshot"
)

func TestCheckCreateArgs(t *testing.T) {
	for _, c := range []struct {
		desc         string
		name         string
		fromSnapshot bool
		version      string
		want         string // substring of the error; "" means no error
	}{
		{"install", "a", false, "4.22", ""},
		{"restore", "a", true, "", ""},
		{"no version", "a", false, "", "--version is required"},
		{"no name to restore", "", true, "", "--from-snapshot restores a vSNO under its own name"},
	} {
		err := checkCreateArgs(c.name, c.fromSnapshot, c.version)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v, want nil", c.desc, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: nil, want an error containing %q", c.desc, c.want)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: %v, want %q", c.desc, err, c.want)
		}
	}
}

func TestRestoreTarget(t *testing.T) {
	if got, err := restoreTarget(&snapshot.Entry{Name: "a"}, "a"); err != nil || got != "a" {
		t.Errorf("restoreTarget = %q, %v; want a, nil", got, err)
	}

	_, err := restoreTarget(nil, "a")
	if err == nil {
		t.Fatal("a missing snapshot must be an error")
	}
	for _, want := range []string{"no snapshot named a", "vsno snapshot create a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
}

func testCreatePlan() *guest.CreatePlan {
	return &guest.CreatePlan{
		Namespace: "vsno", Name: "seed", Node: "worker-0",
		Release: release.Release{Requested: "4.22", Version: "4.22.18", Image: "quay.io/x:4.22.18"},
		Cores:   16, MemoryGiB: 32, DiskGiB: 200,
		Cache: &guest.CacheState{Repo: "ocp/release/abc"},
		Steps: []guest.PlanStep{
			{Name: "Service and DNS", Detail: "api.seed.vsno.lab"},
			{Name: "Cache the release", Detail: "already mirrored: ocp/release/abc"},
			{Name: "Create the VM", Detail: "seed on worker-0"},
		},
	}
}

func TestRenderCreatePlan(t *testing.T) {
	var b bytes.Buffer
	if err := renderCreatePlan(&b, testCreatePlan(), plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"vSNO seed in vsno on worker-0", "4.22.18, 16 vCPU / 32 GiB / 200 GiB", "cache hit",
		"quay.io/x:4.22.18", "STEP", "DETAIL", "Create the VM", "Dry run: nothing was changed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderCreatePlanShader(t *testing.T) {
	for _, c := range []struct {
		name string
		p    *guest.CreatePlan
		want string
	}{
		{"no cache", &guest.CreatePlan{NoCache: true, Steps: nil}, "no cache"},
		{"cache miss", &guest.CreatePlan{Steps: nil}, "cache miss"},
		{"snapshot", &guest.CreatePlan{Snapshot: "seed", Steps: nil}, "restored from snapshot seed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := renderCreatePlan(&b, c.p, plain); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), c.want) {
				t.Errorf("missing %q in:\n%s", c.want, b.String())
			}
		})
	}
}

func TestRenderCreatePlanJSON(t *testing.T) {
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = formatTable })
	var b bytes.Buffer
	if err := renderCreatePlan(&b, testCreatePlan(), plain); err != nil {
		t.Fatal(err)
	}
	var got guest.CreatePlan
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "seed" || len(got.Steps) != 3 || got.Cache == nil || got.Cache.Repo == "" {
		t.Errorf("json = %+v", got)
	}
}
