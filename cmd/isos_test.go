package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/isos"
)

// The parent command lists only when it is given nothing: `vsno iso info seed2`
// used to run list and silently ignore both arguments.
func TestIsoCommandRefusesStrayArgs(t *testing.T) {
	if err := isoCmd.Args(isoCmd, []string{"info", "seed2"}); err == nil {
		t.Error("vsno iso with arguments must error, not list")
	}
	if err := isoCmd.Args(isoCmd, nil); err != nil {
		t.Errorf("vsno iso with no arguments = %v, want nil", err)
	}
}

func TestRenderIsoInfo(t *testing.T) {
	in := &isos.Info{
		Name: "a",
		Entries: []isos.Entry{
			{Kind: isos.Served, Name: "a", Path: "/work/www/a.iso", Size: 1 << 30,
				Modified: time.Now().Add(-2 * time.Hour), Use: isos.InUse, Users: []string{"a"}},
			{Kind: isos.Installer, Name: "openshift-install-x", Path: "/work/bin/openshift-install-x",
				Release: "quay.io/x:4.20.46", Version: "4.20.46", Size: 700 << 20, Use: isos.Unused},
		},
		Disks: []isos.Disk{{Name: "a-iso", VSNO: "a", Size: 3 << 30, Use: isos.InUse, Users: []string{"a"}}},
		Bytes: 1<<30 + 700<<20 + 3<<30,
		Used:  4 << 30, Total: 5 << 30,
	}
	var b bytes.Buffer
	if err := renderIsoInfo(&b, in, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"a:", "1.7Gi in the builder", "3.0Gi on LVMS", "PATH", "AGE",
		"/work/www/a.iso", "2h", "4.20.46", "quay.io/x:4.20.46",
		"data volume", "a-iso", "Builder volume: 4.0Gi of 5.0Gi used (80%)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderIsos(t *testing.T) {
	rep := &isos.Report{
		Entries: []isos.Entry{
			{Kind: isos.Served, Name: "a", Size: 1 << 30, Use: isos.InUse, Users: []string{"a"}},
			{Kind: isos.Build, Name: "gone", Size: 2 << 30, Use: isos.Orphaned},
			{Kind: isos.Installer, Name: "openshift-install-x", Version: "4.20.46", Size: 700 << 20, Use: isos.Unused},
			{Kind: isos.Base, Name: "base-iso", Size: 1 << 30},
		},
		Used: 4 << 30, Total: 5 << 30,
		Disks:     []isos.Disk{{Name: "a-iso", VSNO: "a", Size: 3 << 30, Use: isos.InUse, Users: []string{"a"}}},
		DiskBytes: 3 << 30,
	}
	var b bytes.Buffer
	if err := renderIsos(&b, rep, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"served", "a", "in use (a)", "orphaned", "unused", "4.20.46", "AGE",
		"data volume", "a-iso", "Builder volume: 4.0Gi of 5.0Gi used (80%)",
		"ISO DataVolumes on LVMS: 3.0Gi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	b.Reset()
	if err := renderIsos(&b, &isos.Report{}, plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Nothing in") {
		t.Errorf("empty report = %q", b.String())
	}
}

func TestRenderIsosWide(t *testing.T) {
	old := outputFormat
	t.Cleanup(func() { outputFormat = old })

	rep := &isos.Report{Entries: []isos.Entry{
		{Kind: isos.Served, Name: "a", Path: "/work/www/a.iso", Size: 1 << 30, Use: isos.InUse, Users: []string{"a"}},
	}}
	render := func(format string) string {
		outputFormat = format
		var b bytes.Buffer
		if err := renderIsos(&b, rep, plain); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}

	if table := render(formatTable); strings.Contains(table, "PATH") || strings.Contains(table, "/work/www/a.iso") {
		t.Errorf("table output shows the path:\n%s", table)
	}
	wide := render(formatWide)
	for _, want := range []string{"PATH", "/work/www/a.iso"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide output is missing %q:\n%s", want, wide)
		}
	}
}

func TestRenderPrunePlan(t *testing.T) {
	p := &isos.Plan{
		VSNOs:      []isos.VSNOFiles{{Name: "gone", Size: 4 << 30}},
		Installers: []isos.Entry{{Name: "openshift-install-x", Version: "4.19.9", Size: 700 << 20}},
		Disks:      []isos.Disk{{Name: "gone-iso", Size: 3 << 30}},
		Bytes:      5 << 30,
	}
	var b bytes.Buffer
	renderPrunePlan(&b, p, plain)
	out := b.String()
	for _, want := range []string{"vSNO gone's ISO", "openshift-install-x", "4.19.9", "DataVolume gone-iso", "Frees 5.0Gi"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
