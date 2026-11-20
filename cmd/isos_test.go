package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/isos"
)

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
		"served", "a", "in use (a)", "orphaned", "unused", "4.20.46",
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

func TestRenderPrunePlan(t *testing.T) {
	p := &isos.Plan{
		VSNOs:      []string{"gone"},
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
