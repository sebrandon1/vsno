package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
)

func TestCacheUsers(t *testing.T) {
	gs := []guest.Guest{
		{Name: "cached", Image: "img", Cached: true},
		{Name: "plain", Image: "img"},
		{Name: "nameless", Cached: true},
	}
	users := cacheUsers(gs)
	if len(users) != 1 || len(users["img"]) != 1 || users["img"][0] != "cached" {
		t.Errorf("users = %v", users)
	}
}

func TestRenderCache(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rep := &cache.Report{
		Entries: []cache.Entry{
			{Version: "4.20.46", Image: "quay.io/x:4.20.46", SizeBytes: 6 << 30,
				Mirrored: now.Add(-2 * time.Hour), LastUsed: now.Add(-time.Hour), Users: []string{"a", "b"}},
			{Version: "4.21.0", Image: "quay.io/x:4.21.0", SizeBytes: 3 << 30,
				Mirrored: now.Add(-48 * time.Hour), LastUsed: now.Add(-48 * time.Hour)},
			{Version: "4.21.42", Image: "quay.io/x:4.21.42", State: cache.StateMirroring, Started: now.Add(-2 * time.Minute)},
		},
		Used: 40 << 30, Total: 100 << 30,
	}
	var b bytes.Buffer
	if err := renderCache(&b, rep, now, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"VERSION", "STATE", "4.20.46", "quay.io/x:4.20.46", "6.0Gi", "a, b",
		"ready", "mirroring 2m", "registry volume: 40.0Gi of 100.0Gi used, 60.0Gi free"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A release no vSNO uses shows "-", not an empty cell.
	if !strings.Contains(out, "4.21.0") {
		t.Errorf("second entry missing:\n%s", out)
	}

	b.Reset()
	if err := renderCache(&b, &cache.Report{}, now, plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "No releases cached") {
		t.Errorf("empty report = %q", b.String())
	}
}
