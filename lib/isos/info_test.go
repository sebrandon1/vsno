package isos

import (
	"strings"
	"testing"
)

// findReport is a listing with one vSNO (its served ISO, build directory and
// DataVolume), an installer, an orphaned build directory, and the base cache.
func findReport() *Report {
	return &Report{
		Entries: []Entry{
			{Kind: Served, Name: "a", Path: "/work/www/a.iso", Size: 1 << 30, Use: InUse, Users: []string{"a"}},
			{Kind: Build, Name: "a", Path: "/work/guests/a", Size: 2 << 20, Use: InUse, Users: []string{"a"}},
			{Kind: Build, Name: "gone", Path: "/work/guests/gone", Size: 1 << 20, Use: Orphaned},
			{Kind: Installer, Name: "openshift-install-x", Path: "/work/bin/openshift-install-x", Release: "quay.io/x:4.20.46", Version: "4.20.46", Size: 700 << 20, Use: InUse, Users: []string{"a"}},
			{Kind: Base, Name: "base-iso", Path: "/work/cache", Size: 1 << 30},
		},
		Disks: []Disk{{Name: "a-iso", VSNO: "a", Size: 3 << 30, Use: InUse, Users: []string{"a"}}},
		Used:  4 << 30, Total: 5 << 30,
	}
}

func TestFindVSNO(t *testing.T) {
	in, err := Find(findReport(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Entries) != 2 {
		t.Errorf("entries = %d, want the served ISO and the build directory", len(in.Entries))
	}
	if len(in.Disks) != 1 || in.Disks[0].Name != "a-iso" {
		t.Errorf("disks = %+v, want a-iso", in.Disks)
	}
	if want := int64(1<<30 + 2<<20 + 3<<30); in.Bytes != want {
		t.Errorf("bytes = %d, want %d", in.Bytes, want)
	}
	if in.DiskBytes() != 3<<30 {
		t.Errorf("disk bytes = %d, want 3Gi", in.DiskBytes())
	}
	if in.Used != 4<<30 || in.Total != 5<<30 {
		t.Errorf("volume = %d/%d, want the report's", in.Used, in.Total)
	}
}

func TestFindByInstallerAndDataVolume(t *testing.T) {
	for _, name := range []string{"openshift-install-x", "a-iso"} {
		in, err := Find(findReport(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(in.Entries)+len(in.Disks) != 1 {
			t.Errorf("%s: found %d entries and %d disks, want exactly it", name, len(in.Entries), len(in.Disks))
		}
	}
}

func TestFindOrphanedBuild(t *testing.T) {
	in, err := Find(findReport(), "gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Entries) != 1 || in.Entries[0].Use != Orphaned {
		t.Errorf("entries = %+v, want the orphaned build directory", in.Entries)
	}
}

func TestFindUnknownName(t *testing.T) {
	_, err := Find(findReport(), "nope")
	if err == nil {
		t.Fatal("want an error for a name nothing holds")
	}
	for _, want := range []string{"nothing named nope", "vsno iso list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
}

// Nothing in use is refused: unlike Delete, info exists to look at it.
func TestFindNeverRefuses(t *testing.T) {
	in, err := Find(findReport(), "base-iso")
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Entries) != 1 || in.Entries[0].Path != "/work/cache" {
		t.Errorf("entries = %+v, want the base cache with its path", in.Entries)
	}
}
