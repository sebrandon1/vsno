package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/seed"
)

// seedImages is one finished seed image and one whose version label is unset.
func seedImages() []seed.Image {
	now := time.Now()
	return []seed.Image{
		{Ref: "example.test/vsno/seed-a:4.20.46", From: "seed-a", Version: "4.20.46", SizeBytes: 3 << 30, Created: now.Add(-2 * time.Hour)},
		{Ref: "example.test/vsno/seed-b:latest", From: "seed-b", SizeBytes: 700 << 20, Created: now.Add(-30 * time.Minute)},
	}
}

func TestRenderSeeds(t *testing.T) {
	var b bytes.Buffer
	if err := renderSeeds(&b, seedImages(), time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"IMAGE", "FROM", "VERSION", "SIZE", "AGE",
		"seed-a", "4.20.46", "3.0Gi", "2h",
		"seed-b", "700Mi", "30m", "-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderSeedsEmpty(t *testing.T) {
	var b bytes.Buffer
	if err := renderSeeds(&b, nil, time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "No seed images") {
		t.Errorf("empty seeds = %q, want the create hint", b.String())
	}
}

// A nil listing is [] in JSON, not null: scripts read it as a list.
func TestRenderSeedsJSONEmpty(t *testing.T) {
	old := outputFormat
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = old })

	var b bytes.Buffer
	if err := renderSeeds(&b, nil, time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(b.String()); got != "[]" {
		t.Errorf("json = %q, want []", got)
	}
}

func TestRenderSeedPlan(t *testing.T) {
	var b bytes.Buffer
	renderSeedPlan(&b, seedImages(), plain)
	out := b.String()
	for _, want := range []string{"delete", "seed-a", "3.0Gi", "seed-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	b.Reset()
	renderSeedPlan(&b, nil, plain)
	if b.Len() != 0 {
		t.Errorf("empty plan printed %q, want nothing", b.String())
	}
}

func TestRenderSeedSync(t *testing.T) {
	res := &seed.SyncResult{
		Name: "seed-a", Version: "4.20.46",
		Source:      "registry.vsno.svc.cluster.local:5000/seeds/seed-a:4.20.46",
		Destination: "quay.io/me/seeds:4.20.46",
		Digest:      "sha256:aa",
		SizeBytes:   3 << 30,
	}
	for _, want := range []string{"synced seed-a:4.20.46", "3.0Gi", "from", res.Source, "to", res.Destination, "sha256:aa"} {
		var b bytes.Buffer
		renderSeedSync(&b, res, plain)
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}

	// Without a digest there is no digest line rather than an empty one.
	var b bytes.Buffer
	renderSeedSync(&b, &seed.SyncResult{Name: "seed-a", Source: "s", Destination: "d"}, plain)
	if strings.Contains(b.String(), "digest") {
		t.Errorf("no digest, but printed one:\n%s", b.String())
	}
}

// A seed copied in shows when it arrived, and says so; one made here shows its
// own age.
func TestSeedAgePrefersTheSyncTime(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1201 * 24 * time.Hour)
	synced := now.Add(-5 * time.Minute)
	img := seed.Image{Created: old}
	if got, want := seedAge(now, img), "1201d"; got != want {
		t.Errorf("no sync: %q, want %q", got, want)
	}
	img.SyncedAt = &synced
	if got, want := seedAge(now, img), "5m (synced)"; got != want {
		t.Errorf("synced: %q, want %q", got, want)
	}
}
