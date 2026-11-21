package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/ibu"
	"github.com/sebrandon1/vsno/lib/seed"
)

func upgradeResult() *ibu.UpgradeResult {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	res := &ibu.UpgradeResult{
		Target: "t", SeedImage: "registry/seeds/shiny-crane:4.21.42", SeedVersion: "4.21.42",
		Before: ibu.Identity{Version: "4.21.1", ClusterID: "c1", InfrastructureName: "i1", Node: "t", APIURL: "https://api.t.vsno.lab:6443"},
		After:  ibu.Identity{Version: "4.21.42", ClusterID: "c1", InfrastructureName: "i1", Node: "t", APIURL: "https://api.t.vsno.lab:6443"},
		Stage:  ibu.StageUpgrade,
		History: []ibu.History{
			{Stage: ibu.StagePrep, StartTime: t0, CompletionTime: t0.Add(6 * time.Minute)},
			{Stage: ibu.StageUpgrade, StartTime: t0.Add(7 * time.Minute), CompletionTime: t0.Add(24 * time.Minute)},
		},
	}
	res.History[1].Phases = []ibu.Phase{{Phase: "Restore", StartTime: t0.Add(20 * time.Minute)}}
	return res
}

func TestRenderUpgrade(t *testing.T) {
	var b bytes.Buffer
	renderUpgrade(&b, upgradeResult(), ibu.UpgradeOptions{Seed: "shiny-crane"}, plain)
	out := b.String()
	for _, want := range []string{
		"Seed registry/seeds/shiny-crane:4.21.42 (OpenShift 4.21.42)",
		"version             4.21.1", "4.21.42",
		"Prep", "6m", "Upgrade", "17m", "Restore", "running",
		"The target is in stage Upgrade.",
		"`vsno upgrade t --seed shiny-crane --finalize`", "--rollback`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "changed:") {
		t.Errorf("an unchanged identity reported as changed:\n%s", out)
	}
}

func TestRenderUpgradeChangedIdentity(t *testing.T) {
	res := upgradeResult()
	res.Changed = []string{"clusterID c1 → c2"}
	res.Warnings = []string{"can't read the seed vSNO"}
	var b bytes.Buffer
	renderUpgrade(&b, res, ibu.UpgradeOptions{Seed: "x"}, plain)
	for _, want := range []string{"✗ changed: clusterID c1 → c2", "! can't read the seed vSNO"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q:\n%s", want, b.String())
		}
	}
}

func TestNextStep(t *testing.T) {
	o := ibu.UpgradeOptions{Seed: "s"}
	for stage, want := range map[ibu.Stage]string{
		ibu.StagePrep:     "Continue with `vsno upgrade t --seed s`.",
		ibu.StageUpgrade:  "Keep it with `vsno upgrade t --seed s --finalize`",
		ibu.StageIdle:     "",
		ibu.StageRollback: "",
	} {
		got := nextStep(&ibu.UpgradeResult{Target: "t", Stage: stage}, o)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", stage, got, want)
		}
	}
}

func TestUpgradeFlags(t *testing.T) {
	upTo = "later"
	defer func() { upTo = "upgrade" }()
	if err := runUpgrade(upgradeCmd, []string{"t"}); err == nil || !strings.Contains(err.Error(), "--to takes prep or upgrade") {
		t.Errorf("err = %v", err)
	}
	prRole = "both"
	defer func() { prRole = string(ibu.RoleTarget) }()
	if err := runPrepare(prepareCmd, []string{"t"}); err == nil || !strings.Contains(err.Error(), "--role takes seed or target") {
		t.Errorf("err = %v", err)
	}
}

func TestDetailText(t *testing.T) {
	for _, c := range []struct {
		g    guest.Guest
		want string
	}{
		{guest.Guest{}, ""},
		{guest.Guest{Detail: "installing 35%"}, "installing 35%"},
		{guest.Guest{IBU: "Prep"}, "IBU Prep"},
		{guest.Guest{IBU: "Upgrade done", Detail: "1 operator(s) degraded or unavailable"}, "IBU Upgrade done; 1 operator(s) degraded or unavailable"},
	} {
		if got := detailText(c.g); got != c.want {
			t.Errorf("%+v = %q, want %q", c.g, got, c.want)
		}
	}
}

// A seed image without a creation time shows "-" for its age, not 106751d.
func TestRenderSeedsWithoutCreated(t *testing.T) {
	var b bytes.Buffer
	imgs := []seed.Image{{Ref: "reg/seeds/a:4.22.22", From: "a", Version: "4.22.22", SizeBytes: 1 << 30}}
	if err := renderSeeds(&b, imgs, time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "1.0Gi  -") || strings.Contains(out, "106751d") {
		t.Errorf("seed list:\n%s", out)
	}
}
