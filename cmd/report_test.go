package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/report"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/status"
)

func reportFixture() *report.Report {
	now := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)
	return &report.Report{
		Namespace: "vsno",
		Status:    &status.Report{Namespace: "vsno", Exists: true, Next: "upgrade with `vsno upgrade a --seed b`"},
		VSNOs: []guest.Guest{{Name: "a", State: guest.Ready, Version: "4.22.23", Created: now.Add(-2 * time.Hour)},
			{Name: "b", State: guest.Stopped, Version: "4.22.22"}},
		Cache: &cache.Report{Used: 65 << 30, Total: 100 << 30, Entries: []cache.Entry{{Version: "4.22.23", SizeBytes: 21 << 30}}},
		Seeds: []seed.Image{{Ref: "reg/seeds/a:4.22.23", Version: "4.22.23", SizeBytes: 3 << 29}},
	}
}

func TestRenderReportMarkdown(t *testing.T) {
	var b bytes.Buffer
	if err := renderReportMarkdown(&b, reportFixture(), time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# vsno report: vsno", "healthy: true", "next: upgrade with",
		"## vSNOs", "| a | ready | 4.22.23 | 2h |", "| b | stopped | 4.22.22 | - |",
		"## Release cache", "4.22.23", "21.0Gi", "65.0Gi of 100.0Gi",
		"## Seed images", "reg/seeds/a:4.22.23", "1.5Gi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\033[") {
		t.Error("the Markdown carries ANSI codes")
	}
}

func TestReportWarningsRender(t *testing.T) {
	rep := reportFixture()
	rep.Warnings = []string{"the builder's files: no builder"}
	var b bytes.Buffer
	if err := renderReportMarkdown(&b, rep, time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "## Warnings") || !strings.Contains(b.String(), "no builder") {
		t.Errorf("warnings missing:\n%s", b.String())
	}
}
