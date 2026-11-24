package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/snapshot"
)

func intp(n int) *int { return &n }

// testDescription is one vSNO with everything describe shows filled in.
func testDescription() *vsnoDescription {
	return &vsnoDescription{
		VSNO: guest.Guest{
			Namespace: "vsno", Name: "seed", State: guest.Ready, Detail: "1 operator(s) degraded",
			Version: "4.22.18", Requested: "4.22", Image: "quay.io/x:4.22.18",
			Node: "worker-0", Cores: 16, MemoryGiB: 32, DiskGiB: 200,
			ServiceIP: "10.0.0.5", API: "https://api.seed.vsno.lab:6443",
			Created:   time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC),
			Unhealthy: intp(1), Cached: true,
			CVMessage: "Cluster operator X is degraded",
			Timings:   guest.Timings{ISO: "8m12s", Install: "14m3s"},
		},
		Seeds: []seed.Image{{Ref: "registry.vsno.svc/seeds/seed:4.22.18", From: "seed", Version: "4.22.18", SizeBytes: 3 << 30}},
		Snapshot: &snapshot.Entry{Name: "seed", VSNO: "seed", Version: "4.22.18", SizeBytes: 2 << 30,
			Created: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), InUse: true},
		Next: "make a seed",
	}
}

func TestFindGuest(t *testing.T) {
	gs := []guest.Guest{{Name: "a"}, {Name: "b"}}
	if g, err := findGuest(gs, "b"); err != nil || g.Name != "b" {
		t.Errorf("findGuest = %+v, %v", g, err)
	}
	if _, err := findGuest(gs, "ghost"); err == nil ||
		!strings.Contains(err.Error(), "no vSNO named ghost") || !strings.Contains(err.Error(), "vsno list") {
		t.Errorf("missing = %v", err)
	}
}

func TestSeedsFrom(t *testing.T) {
	imgs := []seed.Image{{Ref: "a", From: "seed"}, {Ref: "b", From: "target"}, {Ref: "c", From: "seed"}}
	got := seedsFrom(imgs, "seed")
	if len(got) != 2 || got[0].Ref != "a" || got[1].Ref != "c" {
		t.Errorf("seedsFrom = %+v", got)
	}
	if got := seedsFrom(imgs, "none"); got != nil {
		t.Errorf("no seeds = %+v, want nil", got)
	}
}

func TestSnapshotFor(t *testing.T) {
	newest := snapshot.Entry{Name: "seed", VSNO: "seed", Created: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	older := snapshot.Entry{Name: "seed-old", VSNO: "seed", Created: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)}
	other := snapshot.Entry{Name: "target", VSNO: "target"}
	if got := snapshotFor([]snapshot.Entry{newest, older, other}, "seed"); got == nil || got.Name != "seed" {
		t.Errorf("snapshotFor = %+v, want the newest seed snapshot", got)
	}
	if got := snapshotFor([]snapshot.Entry{other}, "seed"); got != nil {
		t.Errorf("no snapshot = %+v, want nil", got)
	}
}

func TestUnhealthyText(t *testing.T) {
	for _, c := range []struct {
		n    *int
		want string
	}{{nil, "-"}, {intp(0), "0 operators"}, {intp(2), "2 operator(s)"}} {
		if got := unhealthyText(c.n); got != c.want {
			t.Errorf("unhealthyText(%v) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestTimingsText(t *testing.T) {
	if got := timingsText(guest.Timings{}); got != "-" {
		t.Errorf("empty timings = %q", got)
	}
	if got := timingsText(guest.Timings{ISO: "8m12s", Install: "14m3s"}); got != "iso 8m12s, install 14m3s" {
		t.Errorf("timings = %q", got)
	}
}

func TestDescribeSizeAndAPI(t *testing.T) {
	if got := describeSize(guest.Guest{}); got != "-" {
		t.Errorf("size = %q", got)
	}
	if got := describeSize(guest.Guest{Cores: 16, MemoryGiB: 32, DiskGiB: 200}); got != "16 vCPU / 32 GiB memory / 200 GiB disk" {
		t.Errorf("size = %q", got)
	}
	if got := describeAPI(guest.Guest{API: "https://api.a:6443"}); got != "https://api.a:6443" {
		t.Errorf("api = %q", got)
	}
	if got := describeAPI(guest.Guest{API: "https://api.a:6443", ServiceIP: "10.0.0.1"}); got != "https://api.a:6443 (10.0.0.1)" {
		t.Errorf("api with ip = %q", got)
	}
}

func TestRenderDescribe(t *testing.T) {
	var b bytes.Buffer
	if err := renderDescribe(&b, testDescription(), time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"vSNO seed in vsno", "state", "ready", "detail", "1 operator(s) degraded",
		"cv message", "Cluster operator X is degraded", "version", "4.22.18", "requested", "4.22",
		"image", "quay.io/x:4.22.18", "size", "16 vCPU / 32 GiB memory / 200 GiB disk",
		"node", "worker-0", "api", "https://api.seed.vsno.lab:6443 (10.0.0.5)",
		"cached", "yes", "lca", "catalog", "unhealthy", "1 operator(s)", "timings", "iso 8m12s",
		"Seed images", "registry.vsno.svc/seeds/seed:4.22.18", "Snapshot", "vSNO seed exists",
		"Next:", "make a seed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderDescribeWarnings(t *testing.T) {
	d := testDescription()
	d.Seeds, d.Snapshot, d.Next = nil, nil, ""
	d.Warnings = []string{"seed images: registry down"}
	var b bytes.Buffer
	if err := renderDescribe(&b, d, time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "registry down") || strings.Contains(out, "Seed images") {
		t.Errorf("warnings:\n%s", out)
	}
}

func TestRenderDescribeJSON(t *testing.T) {
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = formatTable })
	var b bytes.Buffer
	if err := renderDescribe(&b, testDescription(), time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	var got struct {
		VSNO     guest.Guest     `json:"vsno"`
		Seeds    []seed.Image    `json:"seeds"`
		Snapshot *snapshot.Entry `json:"snapshot"`
		Next     string          `json:"next"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.VSNO.Name != "seed" || len(got.Seeds) != 1 || got.Snapshot == nil || got.Next != "make a seed" {
		t.Errorf("json = %+v", got)
	}
}

func TestDescribeCreated(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if got := describeCreated(now, guest.Guest{}); got != "-" {
		t.Errorf("no creation time = %q, want -", got)
	}
	got := describeCreated(now, guest.Guest{Created: now.Add(-2 * time.Hour)})
	if !strings.Contains(got, "2026-11-24T10:00:00Z") || !strings.Contains(got, "(2h)") {
		t.Errorf("created = %q", got)
	}
}
