package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/snapshot"
)

// snapshotEntries is one snapshot whose vSNO still exists and one spare one.
func snapshotEntries() []snapshot.Entry {
	now := time.Now()
	return []snapshot.Entry{
		{Name: "a", VSNO: "a", Version: "4.20.46", SizeBytes: 3 << 30, Created: now.Add(-2 * time.Hour), InUse: true},
		{Name: "b", VSNO: "b", SizeBytes: 1 << 30, Created: now.Add(-30 * time.Minute)},
	}
}

func TestRenderSnapshots(t *testing.T) {
	var b bytes.Buffer
	renderSnapshots(&b, snapshotEntries(), time.Now(), plain)
	out := b.String()
	for _, want := range []string{
		"NAME", "VSNO", "VERSION", "SIZE", "AGE", "STATE",
		"a", "4.20.46", "3.0Gi", "2h", "vSNO a exists",
		"b", "1.0Gi", "30m", "restorable", "-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderSnapshotsEmpty(t *testing.T) {
	var b bytes.Buffer
	renderSnapshots(&b, nil, time.Now(), plain)
	if !strings.Contains(b.String(), "No snapshots.") {
		t.Errorf("empty snapshots = %q, want the create hint", b.String())
	}
}

func TestRenderSnapshotPlan(t *testing.T) {
	var b bytes.Buffer
	renderSnapshotPlan(&b, snapshot.Deleted{Snapshots: snapshotEntries()}, plain)
	out := b.String()
	for _, want := range []string{"Snapshots to delete", "delete", "a", "3.0Gi", "b"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	b.Reset()
	renderSnapshotPlan(&b, snapshot.Deleted{}, plain)
	if b.Len() != 0 {
		t.Errorf("empty plan printed %q, want nothing", b.String())
	}
}

func TestSnapshotDeleteNamesSnapshots(t *testing.T) {
	err := snapshotDeleteCmd.Args(snapshotDeleteCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "snapshots to delete") ||
		!strings.Contains(err.Error(), "vsno snapshot list") {
		t.Errorf("delete's argument error = %v, want it to name the snapshots", err)
	}
}

func TestOrDefault(t *testing.T) {
	for _, c := range []struct{ s, def, want string }{
		{"", "the only vSNO", "the only vSNO"},
		{"a", "the only vSNO", "a"},
		{"", "", ""},
	} {
		if got := orDefault(c.s, c.def); got != c.want {
			t.Errorf("orDefault(%q, %q) = %q, want %q", c.s, c.def, got, c.want)
		}
	}
}
