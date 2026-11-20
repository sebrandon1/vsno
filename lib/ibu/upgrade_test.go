package ibu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/precheck"
)

func upgrade(t *testing.T, target *lca, o UpgradeOptions, extra map[string]guestctl.Access) (*UpgradeResult, error) {
	t.Helper()
	access := map[string]guestctl.Access{"t": target}
	for k, v := range extra {
		access[k] = v
	}
	o.Namespace, o.Target = ns, "t"
	if o.Seed == "" {
		o.Seed = "shiny-crane"
	}
	return Upgrade(context.Background(), testEnv(access), o)
}

func TestUpgradeFinalizes(t *testing.T) {
	target := &lca{Fake: targetFake(), reboot: 2}
	res, err := upgrade(t, target, UpgradeOptions{Finalize: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The newest of the vSNO's seeds, with the version from its label.
	if res.SeedImage != seedRef || res.SeedVersion != "4.21.42" {
		t.Errorf("seed = %s %s", res.SeedImage, res.SeedVersion)
	}
	if res.Before.Version != "4.21.1" || res.After.Version != "4.21.42" || len(res.Changed) != 0 {
		t.Errorf("before = %+v, after = %+v, changed = %q", res.Before, res.After, res.Changed)
	}
	if res.Stage != StageIdle || len(res.History) == 0 {
		t.Errorf("ended in %s with history %+v", res.Stage, res.History)
	}
	want := []string{
		`{"spec":{"seedImageRef":{"image":"` + seedRef + `","pullSecretRef":{"name":"seed-pull-secret"},"version":"4.21.42"},"stage":"Prep"}}`,
		`{"spec":{"stage":"Upgrade"}}`,
		`{"spec":{"stage":"Idle"}}`,
	}
	if strings.Join(target.patches, "\n") != strings.Join(want, "\n") {
		t.Errorf("patches =\n%s\nwant\n%s", strings.Join(target.patches, "\n"), strings.Join(want, "\n"))
	}
	// Prepare ran: the backup location and the seed pull secret were applied.
	if len(target.Stdin) < 2 || !strings.Contains(strings.Join(target.Stdin, ""), "DataProtectionApplication") ||
		!strings.Contains(strings.Join(target.Stdin, ""), "seed-pull-secret") {
		t.Errorf("applied = %q", target.Stdin)
	}
}

func TestUpgradeStopsAtPrep(t *testing.T) {
	target := &lca{Fake: targetFake()}
	res, err := upgrade(t, target, UpgradeOptions{To: StagePrep}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stage != StagePrep || len(target.patches) != 1 || res.After.Version != "" {
		t.Errorf("stage = %s, patches = %q, after = %+v", res.Stage, target.patches, res.After)
	}
}

func TestUpgradeRollsBackWhenAsked(t *testing.T) {
	target := &lca{Fake: targetFake()}
	res, err := upgrade(t, target, UpgradeOptions{Rollback: true}, nil)
	if err != nil || res.Stage != StageRollback || !strings.Contains(target.patches[len(target.patches)-1], `"Rollback"`) {
		t.Errorf("err = %v, stage = %s, patches = %q", err, res.Stage, target.patches)
	}
}

func TestUpgradeFailures(t *testing.T) {
	for _, c := range []struct {
		name   string
		target *lca
		want   string
	}{
		{"prep fails", &lca{Fake: targetFake(), prepFails: true}, "Prep failed: failed to pull the seed image"},
		{"lca rolls back by itself", &lca{Fake: targetFake(), rollsBack: true}, "LCA rolled the upgrade back: lca-init-monitor timed out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := upgrade(t, c.target, UpgradeOptions{}, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if res.Stage == "" {
				t.Error("a failed upgrade should still report where the target is")
			}
		})
	}
}

// An upgrade that's part way with the same seed carries on without asking LCA
// for Prep again; with another seed it is refused before anything changes.
func TestUpgradeResumes(t *testing.T) {
	target := &lca{Fake: targetFake(), stage: StageUpgrade, image: seedRef}
	res, err := upgrade(t, target, UpgradeOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.patches) != 0 || res.Before.ClusterID != "" || res.Changed != nil {
		t.Errorf("patches = %q, before = %+v", target.patches, res.Before)
	}
	other := &lca{Fake: targetFake(), stage: StagePrep, image: "quay.io/x/seed:1"}
	if _, err := upgrade(t, other, UpgradeOptions{}, nil); err == nil || !strings.Contains(err.Error(), "finish or roll that back") || len(other.patches) != 0 {
		t.Errorf("err = %v, patches = %q", err, other.patches)
	}
}

func TestUpgradeSeed(t *testing.T) {
	lcaV := func(csv string) *guestctltest.Fake {
		return &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{lcaCSVs: {{Out: `{"items":[{"metadata":{"name":"` + csv + `"},"status":{"phase":"Succeeded"}}]}`}}}}
	}
	t.Run("same LCA", func(t *testing.T) {
		res, err := upgrade(t, &lca{Fake: targetFake()}, UpgradeOptions{}, map[string]guestctl.Access{"shiny-crane": lcaV("lifecycle-agent.v4.21.5")})
		if err != nil || len(res.Warnings) != 0 {
			t.Errorf("err = %v, warnings = %q", err, res.Warnings)
		}
	})
	t.Run("different LCA", func(t *testing.T) {
		_, err := upgrade(t, &lca{Fake: targetFake()}, UpgradeOptions{}, map[string]guestctl.Access{"shiny-crane": lcaV("lifecycle-agent.v4.20.9")})
		if err == nil || !strings.Contains(err.Error(), "must be the same version") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("seed vSNO stopped", func(t *testing.T) {
		res, err := upgrade(t, &lca{Fake: targetFake()}, UpgradeOptions{}, nil)
		if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "can't read the seed vSNO shiny-crane") {
			t.Errorf("err = %v, warnings = %q", err, res.Warnings)
		}
	})
	t.Run("a tag of the vSNO", func(t *testing.T) {
		res, err := upgrade(t, &lca{Fake: targetFake()}, UpgradeOptions{Seed: "shiny-crane:4.21.1"}, nil)
		if err != nil || res.SeedVersion != "4.21.1" {
			t.Errorf("err = %v, version = %s", err, res.SeedVersion)
		}
	})
	t.Run("an image elsewhere", func(t *testing.T) {
		res, err := upgrade(t, &lca{Fake: targetFake()}, UpgradeOptions{Seed: "quay.io/me/seed:4.21.42", SeedVersion: "4.21.42"}, nil)
		if err != nil || res.SeedImage != "quay.io/me/seed:4.21.42" {
			t.Errorf("err = %v, image = %s", err, res.SeedImage)
		}
	})
	t.Run("an image elsewhere without its version", func(t *testing.T) {
		_, err := upgrade(t, &lca{Fake: targetFake()}, UpgradeOptions{Seed: "quay.io/me/seed:4.21.42"}, nil)
		if err == nil || !strings.Contains(err.Error(), "--seed-version") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestUpgradeReadFailures(t *testing.T) {
	t.Run("no ImageBasedUpgrade yet", func(t *testing.T) {
		f := targetFake()
		_, err := Upgrade(context.Background(), testEnv(map[string]guestctl.Access{"t": f}), UpgradeOptions{Namespace: ns, Target: "t", Seed: "shiny-crane"})
		if err == nil || !strings.Contains(err.Error(), "LCA has not created its ImageBasedUpgrade") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("API down for longer than the stage allows", func(t *testing.T) {
		target := &lca{Fake: targetFake(), reboot: 1 << 30}
		_, err := upgrade(t, target, UpgradeOptions{}, nil)
		if err == nil || !strings.Contains(err.Error(), "Upgrade didn't finish in") {
			t.Errorf("err = %v", err)
		}
	})
}

// The step reports precache progress as a bar and the API's absence by name.
func TestMoveToReportsProgress(t *testing.T) {
	target := &lca{Fake: targetFake(), stage: StagePrep, image: seedRef}
	ps := &steps{}
	res := &UpgradeResult{SeedImage: seedRef, SeedVersion: "4.21.42"}
	e := testEnv(nil)
	if _, err := moveTo(context.Background(), target, guestctl.Clock{Every: e.Poll, Now: e.Now}, StagePrep, StagePrep, prepWait, res, ps); err != nil {
		t.Fatal(err)
	}
	if ps.done != 2 || ps.total != 4 {
		t.Errorf("progress = %d/%d", ps.done, ps.total)
	}
	up := &lca{Fake: targetFake(), stage: StageUpgrade, image: seedRef, reboot: 2}
	ps = &steps{}
	if _, err := moveTo(context.Background(), up, guestctl.Clock{Every: e.Poll, Now: e.Now}, StageUpgrade, StageUpgrade, upgradeWait, res, ps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(ps.updates, "|"), "is down while the node reboots") {
		t.Errorf("updates = %q", ps.updates)
	}
}

func TestPrepare(t *testing.T) {
	ctx := context.Background()
	t.Run("seed role skips the target's pieces", func(t *testing.T) {
		f := targetFake()
		out, err := Prepare(ctx, testEnv(map[string]guestctl.Access{"s": f}), PrepareOptions{Namespace: ns, Names: []string{"s"}, Role: RoleSeed})
		if err != nil || len(out) != 1 || out[0].LCA != "lifecycle-agent.v4.21.5" || out[0].OADP != "oadp-operator.v1.5.2" {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if f.Ran("oc "+bslGet) || strings.Contains(strings.Join(f.Stdin, ""), "DataProtectionApplication") {
			t.Errorf("seed got the target's backup location: %q", f.Calls)
		}
	})
	t.Run("backup location never Available", func(t *testing.T) {
		f := targetFake()
		f.OcReplies[bslGet] = []guestctltest.Reply{{Out: `{"items":[{"metadata":{"name":"default"},"status":{"phase":"Unavailable"}}]}`}}
		_, err := Prepare(ctx, testEnv(map[string]guestctl.Access{"t": f}), PrepareOptions{Namespace: ns, Names: []string{"t"}, Role: RoleTarget})
		if err == nil || !strings.Contains(err.Error(), "was Unavailable after") || !strings.HasPrefix(err.Error(), "t: ") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("operator install fails", func(t *testing.T) {
		f := targetFake()
		f.OcReplies[oadpCSVs] = []guestctltest.Reply{{Err: errors.New("exit 1")}}
		out, err := Prepare(ctx, testEnv(map[string]guestctl.Access{"t": f}), PrepareOptions{Namespace: ns, Names: []string{"t"}, Role: RoleTarget})
		if err == nil || len(out) != 1 || out[0].LCA == "" {
			t.Errorf("out = %+v, err = %v", out, err)
		}
	})
}

// Without the environment's S3, a target can't be given a backup location.
func TestPrepareNeedsS3(t *testing.T) {
	ctx := context.Background()
	t.Run("no S3", func(t *testing.T) {
		e := testEnv(nil)
		_ = e.Kube.CoreV1().Services(ns).Delete(ctx, "s3", deleteOptions)
		if _, err := Prepare(ctx, e, PrepareOptions{Namespace: ns, Names: []string{"t"}, Role: RoleTarget}); err == nil || !strings.Contains(err.Error(), "S3 Service") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no S3 keys", func(t *testing.T) {
		e := testEnv(nil)
		_ = e.Kube.CoreV1().Secrets(ns).Delete(ctx, "s3-config", deleteOptions)
		if _, err := Prepare(ctx, e, PrepareOptions{Namespace: ns, Names: []string{"t"}, Role: RoleTarget}); err == nil || !strings.Contains(err.Error(), "S3 keys") {
			t.Errorf("err = %v", err)
		}
	})
}

// steps is a ProgressStep that records what it is told.
type steps struct {
	updates     []string
	done, total int
}

func (s *steps) Update(m string)         { s.updates = append(s.updates, m) }
func (s *steps) SetProgress(done, n int) { s.done, s.total = done, n }
func (s *steps) Done(string)             {}
func (s *steps) Warn(string)             {}
func (s *steps) Fail(string)             {}

var _ precheck.ProgressStep = (*steps)(nil)

func TestIdentityReadFailures(t *testing.T) {
	for _, key := range []string{"get clusterversion version", "get infrastructure cluster", "get nodes"} {
		t.Run(key, func(t *testing.T) {
			f := targetFake()
			f.OcReplies[key] = []guestctltest.Reply{{Err: errors.New("exit 1")}}
			if _, err := identity(context.Background(), f); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// A seed image without LCA's labels can't say its version, so it must be given.
func TestUpgradeSeedWithoutVersionLabel(t *testing.T) {
	e := testEnv(map[string]guestctl.Access{"t": &lca{Fake: targetFake()}})
	e.Builder = listing{out: `{"repo":"seeds/plain","tag":"1","digest":"sha256:c","created":"2026-11-19T23:00:00Z","labels":{},"size":1}` + "\n"}
	_, err := Upgrade(context.Background(), e, UpgradeOptions{Namespace: ns, Target: "t", Seed: "plain"})
	if err == nil || !strings.Contains(err.Error(), "no version label; pass --seed-version") {
		t.Errorf("err = %v", err)
	}
	if res, err := Upgrade(context.Background(), e, UpgradeOptions{Namespace: ns, Target: "t", Seed: "plain", SeedVersion: "4.21.42", To: StagePrep}); err != nil || res.SeedVersion != "4.21.42" {
		t.Errorf("with --seed-version: err = %v", err)
	}
}

// OADP creates the backup location a little after the DPA; until then it's
// "not created yet", not a failure.
func TestBackupLocationAppearsLater(t *testing.T) {
	f := targetFake()
	f.OcReplies[bslGet] = []guestctltest.Reply{{Out: `{"items":[]}`}, {Out: `{"items":[{"metadata":{"name":"default"},"status":{"phase":"Available"}}]}`}}
	ps := &steps{}
	e := testEnv(nil)
	s := &backupStore{url: "http://172.30.0.9:9000", accessKey: "k", secretKey: "s"}
	if err := ensureBackupLocation(context.Background(), f, guestctl.Clock{Every: e.Poll, Now: e.Now}, s, ps); err != nil {
		t.Fatal(err)
	}
	if len(ps.updates) != 2 || ps.updates[0] != "backup location: not created yet" || ps.updates[1] != "backup location: Available" {
		t.Errorf("updates = %q", ps.updates)
	}
	f.OcReplies["apply -f -"] = []guestctltest.Reply{{Err: errors.New("exit 1")}}
	if err := ensureBackupLocation(context.Background(), f, guestctl.Clock{Every: e.Poll, Now: e.Now}, s, ps); err == nil {
		t.Error("a failed apply must fail")
	}
}

// A seed made from a vSNO installed from the release cache (it has
// openshift-config/user-ca-bundle) can't upgrade a target installed without
// it, and the other way round: LCA refuses at Prep, so vsno refuses first.
func TestUpgradeRefusesAMismatchedTrustBundle(t *testing.T) {
	bundle := "get configmap user-ca-bundle -n openshift-config --ignore-not-found"
	seedVSNO := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		lcaCSVs: {{Out: `{"items":[{"metadata":{"name":"lifecycle-agent.v4.21.5"},"status":{"phase":"Succeeded"}}]}`}},
	}}
	target := &lca{Fake: targetFake()}
	target.OcReplies[bundle] = []guestctltest.Reply{{Out: `{"metadata":{"name":"user-ca-bundle"}}`}}
	_, err := upgrade(t, target, UpgradeOptions{}, map[string]guestctl.Access{"shiny-crane": seedVSNO})
	if err == nil || !strings.Contains(err.Error(), "the target has openshift-config/user-ca-bundle and the seed vSNO shiny-crane doesn't have") || len(target.patches) != 0 {
		t.Errorf("err = %v, patches = %q", err, target.patches)
	}
	// Both with it: fine.
	seedVSNO.OcReplies[bundle] = []guestctltest.Reply{{Out: `{"metadata":{"name":"user-ca-bundle"}}`}}
	if _, err := upgrade(t, &lca{Fake: target.Fake}, UpgradeOptions{To: StagePrep}, map[string]guestctl.Access{"shiny-crane": seedVSNO}); err != nil {
		t.Errorf("both have it: err = %v", err)
	}
}

func TestUpgradeAbort(t *testing.T) {
	ctx := context.Background()
	inPrep := &lca{Fake: targetFake(), stage: StagePrep, image: seedRef, prepFails: true}
	res, err := Upgrade(ctx, testEnv(map[string]guestctl.Access{"t": inPrep}), UpgradeOptions{Namespace: ns, Target: "t", Abort: true})
	if err != nil || res.Stage != StageIdle || len(inPrep.patches) != 1 || inPrep.patches[0] != `{"spec":{"stage":"Idle"}}` {
		t.Fatalf("err = %v, stage = %s, patches = %q", err, res.Stage, inPrep.patches)
	}
	upgraded := &lca{Fake: targetFake(), stage: StageUpgrade, image: seedRef}
	if _, err := Upgrade(ctx, testEnv(map[string]guestctl.Access{"t": upgraded}), UpgradeOptions{Namespace: ns, Target: "t", Abort: true}); err == nil || !strings.Contains(err.Error(), "use --rollback") || len(upgraded.patches) != 0 {
		t.Errorf("after Upgrade: err = %v, patches = %q", err, upgraded.patches)
	}
	if err := (UpgradeOptions{Target: "t", Abort: true, Seed: "s"}).check(); err == nil || !strings.Contains(err.Error(), "--abort takes only the target") {
		t.Errorf("abort with a seed: err = %v", err)
	}
}
