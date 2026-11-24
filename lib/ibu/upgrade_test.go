package ibu

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/precheck"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// The version an upgrade reaches is recorded on the vSNO's VM, which `vsno list`
// and `vsno snapshot` read.
func TestUpgradeRecordsTheNewVersion(t *testing.T) {
	e := testEnv(map[string]guestctl.Access{"t": &lca{Fake: targetFake(), reboot: 2}})
	if _, err := Upgrade(context.Background(), e, UpgradeOptions{Namespace: ns, Target: "t", Seed: "shiny-crane"}); err != nil {
		t.Fatal(err)
	}
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "t", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := vm.GetAnnotations()[guest.AnnVersion]; got != "4.21.42" {
		t.Errorf("version on the VM = %q, want 4.21.42", got)
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
		// A cross-minor upgrade is the usual cause: say how to give the target
		// the LCA the seed was made with, rather than only what differs.
		for _, want := range []string{"lifecycle-agent.v4.20.9", "the target needs the LCA from the seed's release",
			"vsno prepare t --lca-ref release-4.21 --lca-image lifecycle-agent.v4.20.9",
			"seed vSNO stopped skips this comparison"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want %q in it", err, want)
			}
		}
	})
	t.Run("the same image installed two ways", TestUpgradeSeedSameImageInstalledTwoWays)
	t.Run("seed vSNO stopped", func(t *testing.T) {
		res, err := upgrade(t, &lca{Fake: targetFake()}, UpgradeOptions{}, nil)
		if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "can't read the seed vSNO shiny-crane") {
			t.Errorf("err = %v, warnings = %q", err, res.Warnings)
		}
	})
	t.Run("a tag of the vSNO", func(t *testing.T) {
		// The target must be behind the seed the tag names, or the upgrade is
		// refused before the seed is looked at.
		f := targetFake()
		f.OcReplies["get clusterversion version"] = []guestctltest.Reply{
			{Out: `{"spec":{"clusterID":"c1"},"status":{"desired":{"version":"4.20.46"}}}`}}
		res, err := upgrade(t, &lca{Fake: f}, UpgradeOptions{Seed: "shiny-crane:4.21.1"}, nil)
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
		if err == nil || !strings.Contains(err.Error(), "Upgrade didn't finish in") ||
			!strings.Contains(err.Error(), "last: LCA reported nothing") {
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

// A seed's preparation is seed.EnsureReady's — the steps vsno seed precheck
// --install takes — so the two commands cannot disagree about what it needs.
func TestPrepareSeedTakesSeedPrecheckSteps(t *testing.T) {
	e := testEnv(map[string]guestctl.Access{"s": targetFake()})
	p := &titles{}
	e.Progress = p
	if _, err := Prepare(context.Background(), e, PrepareOptions{Namespace: ns, Names: []string{"s"}, Role: RoleSeed}); err != nil {
		t.Fatal(err)
	}
	want := []string{"s: the Lifecycle Agent", "s: OADP", "s: trusting the environment registry"}
	if !slices.Equal(p.titles, want) {
		t.Errorf("steps %q, want %q", p.titles, want)
	}
}

// titles records the steps a run starts.
type titles struct{ titles []string }

func (t *titles) Start(title string) precheck.ProgressStep {
	t.titles = append(t.titles, title)
	return &steps{}
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
// TestUpgradeSeedSameImageInstalledTwoWays is a cross-minor upgrade done the
// supported way: the seed's LCA from its catalog, the target's deployed from
// the seed's image. The same operator, so the comparison passes.
func TestUpgradeSeedSameImageInstalledTwoWays(t *testing.T) {
	both := targetFake()
	both.OcReplies[lcaDeployGet+" --ignore-not-found"] = []guestctltest.Reply{deployment(lcaImage, 1)}
	seedFake := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		lcaDeployGet + " --ignore-not-found": {deployment(lcaImage, 1)}}}
	res, err := upgrade(t, &lca{Fake: both}, UpgradeOptions{}, map[string]guestctl.Access{"shiny-crane": seedFake})
	if err != nil || len(res.Warnings) != 0 {
		t.Errorf("err = %v, warnings = %q", err, res.Warnings)
	}
}

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
	// Both with it: fine. A fresh target, whose identity reply is still the
	// version behind the seed.
	seedVSNO.OcReplies[bundle] = []guestctltest.Reply{{Out: `{"metadata":{"name":"user-ca-bundle"}}`}}
	both := targetFake()
	both.OcReplies[bundle] = []guestctltest.Reply{{Out: `{"metadata":{"name":"user-ca-bundle"}}`}}
	if _, err := upgrade(t, &lca{Fake: both}, UpgradeOptions{To: StagePrep}, map[string]guestctl.Access{"shiny-crane": seedVSNO}); err != nil {
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

// An abort that LCA's controller will never act on fails in seconds, naming the
// stuck condition and the recovery, rather than polling out the ten minutes.
func TestUpgradeAbortStuckIdle(t *testing.T) {
	target := &lca{Fake: targetFake(), stage: StagePrep, image: seedRef, prepFails: true, idleStuck: true}
	res, err := Upgrade(context.Background(), testEnv(map[string]guestctl.Access{"t": target}), UpgradeOptions{Namespace: ns, Target: "t", Abort: true})
	if err == nil {
		t.Fatal("a stuck Idle transition must fail")
	}
	for _, want := range []string{
		`reason "InProgress"`, "will never start",
		"vsno doctor t --fix",
		`{"op":"replace","path":"/status/conditions/0/reason","value":"Aborting"}`,
		"oc delete pod -n openshift-lifecycle-agent",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "didn't finish in") {
		t.Errorf("it waited out the timeout: %v", err)
	}
	if res.Stage != StageIdle || len(target.patches) != 1 {
		t.Errorf("stage = %s, patches = %q", res.Stage, target.patches)
	}
}

// A controller that is a moment behind the patch must not be called stuck: the
// same condition, once it settles on a reason LCA acts on, is the normal abort.
func TestUpgradeAbortToleratesASlowController(t *testing.T) {
	target := &lca{Fake: targetFake(), stage: StagePrep, image: seedRef, prepFails: true, idleSlow: 2}
	res, err := Upgrade(context.Background(), testEnv(map[string]guestctl.Access{"t": target}), UpgradeOptions{Namespace: ns, Target: "t", Abort: true})
	if err != nil || res.Stage != StageIdle {
		t.Errorf("err = %v, stage = %s", err, res.Stage)
	}
}

// A stuck finalize gets the reason LCA finalizes on, and the patch names the
// condition's own position, not always the first.
func TestStuckIdleError(t *testing.T) {
	u := ibuWith(StageIdle, "", cond("PrepCompleted", "False", "Failed", "Prep failed"),
		cond("UpgradeCompleted", "True", "Completed", "Upgrade completed"),
		cond("Idle", "False", "InProgress", "Abort or finalize not allowed"))
	if _, ok := u.StuckIdle(); !ok {
		t.Fatal("want the condition reported")
	}
	cond, _ := u.StuckIdle()
	err := stuckIdleError("t", u, cond, idleStuckGrace)
	for _, want := range []string{
		`reason "InProgress" ("Abort or finalize not allowed") for 15s`,
		"/status/conditions/2/reason", `value":"Finalizing"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

// A timeout says what LCA last reported when the stage had no message of its
// own, instead of ending at "last:".
func TestMoveToTimeoutReportsTheCondition(t *testing.T) {
	target := &lca{Fake: targetFake(), stage: StageUpgrade, image: seedRef, silentUpgrade: true}
	e := testEnv(nil)
	res := &UpgradeResult{SeedImage: seedRef, SeedVersion: "4.21.42"}
	_, err := moveTo(context.Background(), target, guestctl.Clock{Every: e.Poll, Now: e.Now},
		StageUpgrade, StageUpgrade, time.Second, res, &steps{})
	if err == nil || !strings.Contains(err.Error(), "last: InProgress") {
		t.Errorf("err = %v, want the condition as the last thing LCA said", err)
	}
}

// A prepare that fails on what the vSNO is missing names the command that
// would set it up.
func TestPrepareNamesSeedPrecheck(t *testing.T) {
	// Nothing answers: no LCA CSV, and no lifecycle-agent package in the catalog.
	bare := &guestctltest.Fake{}
	e := testEnv(map[string]guestctl.Access{"s": bare})
	_, err := Prepare(context.Background(), e, PrepareOptions{Namespace: ns, Names: []string{"s"}, Role: RoleSeed})
	if err == nil || !strings.Contains(err.Error(), "vsno seed precheck s --install") {
		t.Errorf("err = %v, want the seed precheck hint", err)
	}
}

// Finalize and rollback end an upgrade that ran. On a target back at Idle they
// must refuse rather than start one: LCA would reject the Prep anyway, and the
// target would be left in Prep.
func TestUpgradeRefusesAnEndStageAtIdle(t *testing.T) {
	for _, c := range []struct {
		o    UpgradeOptions
		want string
	}{
		{UpgradeOptions{Finalize: true}, "there is no upgrade to end with --finalize"},
		{UpgradeOptions{Rollback: true}, "there is no upgrade to end with --rollback"},
	} {
		f := targetFake()
		// The target already runs what the seed supplies.
		f.OcReplies["get clusterversion version"] = []guestctltest.Reply{
			{Out: `{"spec":{"clusterID":"c1"},"status":{"desired":{"version":"4.21.42"}}}`}}
		target := &lca{Fake: f}
		_, err := upgrade(t, target, c.o, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want %q", c.o, err, c.want)
		}
		if len(target.patches) != 0 {
			t.Errorf("%+v: it changed the IBU anyway: %q", c.o, target.patches)
		}
	}
}

// A plain upgrade refuses a seed that is not newer, rather than setting Prep
// and leaving the target there when LCA rejects it.
func TestUpgradeRefusesAnOlderSeed(t *testing.T) {
	for _, c := range []struct {
		name, version, want string
	}{
		{"same version", "4.21.42", "there is no upgrade"},
		{"older seed", "4.21.50", "an IBU only moves forward"},
	} {
		f := targetFake()
		f.OcReplies["get clusterversion version"] = []guestctltest.Reply{
			{Out: `{"spec":{"clusterID":"c1"},"status":{"desired":{"version":"` + c.version + `"}}}`}}
		target := &lca{Fake: f}
		if _, err := upgrade(t, target, UpgradeOptions{}, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
		if len(target.patches) != 0 {
			t.Errorf("%s: it changed the IBU anyway: %q", c.name, target.patches)
		}
	}
}

// The upgrade guard orders plain release versions numerically, and leaves
// anything else (a nightly, an empty version) to LCA.
func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		target, seed string
		cmp          int
		ok           bool
	}{
		{"4.22.22", "4.22.23", -1, true},
		{"4.22.22", "4.22.22", 0, true},
		{"4.22.23", "4.22.22", 1, true},
		{"4.22.9", "4.22.10", -1, true}, // numerically, not as text
		{"4.21.42", "4.22.0", -1, true},
		{"10.0.0", "9.9.9", 1, true},
		{"4.22.22", "4.22.0-0.nightly-2026-11-10-072952", 0, false},
		{"4.22.0-0.nightly-2026-11-10-072952", "4.22.22", 0, false},
		{"", "4.22.22", 0, false},
		{"4.22.22", "", 0, false},
		{"4.22.x", "4.22.23", 0, false},
	} {
		cmp, ok := compareVersions(c.target, c.seed)
		if ok != c.ok || (ok && cmp != c.cmp) {
			t.Errorf("compareVersions(%q, %q) = %d, %v; want %d, %v", c.target, c.seed, cmp, ok, c.cmp, c.ok)
		}
	}
}
