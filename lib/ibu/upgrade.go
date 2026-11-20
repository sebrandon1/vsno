package ibu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
)

// Limits for each stage. Upgrade includes the reboot into the new stateroot
// and its certificate regeneration, about 15 minutes with the API down.
const (
	prepWait     = time.Hour
	upgradeWait  = time.Hour
	finalizeWait = 10 * time.Minute
)

// UpgradeOptions are the user's choices for vsno upgrade.
type UpgradeOptions struct {
	Namespace string
	Target    string // the vSNO to upgrade
	// Seed is a seed image (vsno seed list) or a vSNO, meaning its newest seed.
	Seed string
	// SeedVersion is the seed's OpenShift version, for a seed image outside
	// the environment's registry, whose labels vsno can't read.
	SeedVersion string
	// To stops at a stage: Prep, or Upgrade (the default).
	To Stage
	// Finalize moves to Idle once the upgrade completed, dropping the old
	// stateroot; Rollback goes back to it instead.
	Finalize, Rollback bool
	// Abort moves a target in Prep back to Idle, which is how LCA cancels a
	// Prep, failed or not. It needs no seed.
	Abort bool
	// LCA is where the target's LCA comes from, as for Prepare.
	LCA LCASource
}

// UpgradeResult is a finished, or failed, upgrade.
type UpgradeResult struct {
	Target      string   `json:"target"`
	SeedImage   string   `json:"seedImage"`
	SeedVersion string   `json:"seedVersion"`
	Before      Identity `json:"before"`
	After       Identity `json:"after"`
	// Changed lists identity fields other than the version that changed,
	// which an IBU must not do.
	Changed  []string  `json:"changed,omitempty"`
	Stage    Stage     `json:"stage"` // where the ImageBasedUpgrade ended
	History  []History `json:"history,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
}

// Upgrade takes o.Target through Prep and Upgrade to the seed's version, and
// on to Idle or back with Rollback when asked. Run again on a target that is
// part way with the same seed, it carries on from where the target is.
func Upgrade(ctx context.Context, e Env, o UpgradeOptions) (*UpgradeResult, error) {
	e.Defaults(o.Namespace)
	if o.To == "" {
		o.To = StageUpgrade
	}
	if err := o.check(); err != nil {
		return nil, err
	}
	res := &UpgradeResult{Target: o.Target}
	if o.Abort {
		return abort(ctx, e, o, res)
	}
	a, at, from, err := begin(ctx, e, o, res)
	if err != nil {
		return res, err
	}
	return finish(ctx, a, res, drive(ctx, e, o, a, at, from, res))
}

// begin finds the seed, prepares the target and reads where it is: the stage
// it's at and the one to start from. Starting from Prep, it records the
// target's identity and checks it matches the seed's vSNO (checkSeed).
func begin(ctx context.Context, e Env, o UpgradeOptions, res *UpgradeResult) (a guestctl.Access, at, from Stage, err error) {
	var madeFrom string // the vSNO the seed was made from, when it's one here
	if res.SeedImage, res.SeedVersion, madeFrom, err = resolveSeed(ctx, e, o); err != nil {
		return nil, "", "", err
	}
	if _, err := Prepare(ctx, e, PrepareOptions{Namespace: o.Namespace, Names: []string{o.Target}, Role: RoleTarget, LCA: o.LCA}); err != nil {
		return nil, "", "", err
	}
	a = e.Access(o.Target)
	u, err := current(ctx, a)
	if err != nil {
		return a, "", "", err
	}
	start, err := resumeFrom(u, res.SeedImage)
	if err != nil {
		return a, "", "", err
	}
	if start == StagePrep {
		if res.Before, err = identity(ctx, a); err != nil {
			return a, "", "", err
		}
		if err := checkSeed(ctx, e, madeFrom, o.Target, a, res); err != nil {
			return a, "", "", err
		}
	}
	return a, u.Spec.Stage, start, nil
}

// drive moves the target through the stages from from, up to o.To, then to
// Idle or back with Rollback when asked.
func drive(ctx context.Context, e Env, o UpgradeOptions, a guestctl.Access, at, from Stage, res *UpgradeResult) error {
	c := e.Clock()
	run := func(s Stage, limit time.Duration) error {
		return guestctl.Step(e.Progress, string(s), func(ps precheck.ProgressStep) (string, error) {
			detail, err := moveTo(ctx, a, c, at, s, limit, res, ps)
			at = s
			return detail, err
		})
	}
	if from == StagePrep {
		if err := run(StagePrep, prepWait); err != nil {
			return err
		}
	}
	if o.To == StagePrep {
		return nil
	}
	if err := run(StageUpgrade, upgradeWait); err != nil {
		return err
	}
	after, err := identity(ctx, a)
	if err != nil {
		return err
	}
	res.After = after
	if res.Before.ClusterID != "" {
		res.Changed = res.Before.Changed(after)
	}
	switch {
	case o.Finalize:
		return run(StageIdle, finalizeWait)
	case o.Rollback:
		return run(StageRollback, upgradeWait)
	}
	return nil
}

func (o UpgradeOptions) check() error {
	switch {
	case o.Abort && (o.Finalize || o.Rollback || o.Seed != ""):
		return errors.New("--abort takes only the target: it cancels its Prep")
	case o.Abort:
		return nil
	case o.Target == "" || o.Seed == "":
		return errors.New("a target and a seed are required")
	case o.To != StagePrep && o.To != StageUpgrade:
		return fmt.Errorf("--to takes prep or upgrade, not %s", strings.ToLower(string(o.To)))
	case o.Finalize && o.Rollback:
		return errors.New("--finalize and --rollback are opposites; pick one")
	case o.To == StagePrep && (o.Finalize || o.Rollback):
		return errors.New("--finalize and --rollback come after the upgrade, so they can't go with --to prep")
	}
	return nil
}

// abort moves a target in Prep back to Idle. After Upgrade the way back is
// Rollback, which LCA allows from there instead.
func abort(ctx context.Context, e Env, o UpgradeOptions, res *UpgradeResult) (*UpgradeResult, error) {
	a := e.Access(o.Target)
	u, err := current(ctx, a)
	if err != nil {
		return res, err
	}
	if u.Spec.Stage != StagePrep {
		return finish(ctx, a, res, fmt.Errorf("only Prep can be aborted, and the target is in %s; after Upgrade, use --rollback", orIdle(u.Spec.Stage)))
	}
	err = guestctl.Step(e.Progress, "Aborting Prep", func(ps precheck.ProgressStep) (string, error) {
		return moveTo(ctx, a, e.Clock(), StagePrep, StageIdle, finalizeWait, res, ps)
	})
	return finish(ctx, a, res, err)
}

// orIdle names a stage, the unset one being Idle.
func orIdle(s Stage) Stage {
	if s == "" {
		return StageIdle
	}
	return s
}

// finish records where the target ended, and its history, on any outcome.
func finish(ctx context.Context, a guestctl.Access, res *UpgradeResult, err error) (*UpgradeResult, error) {
	if u, rerr := current(ctx, a); rerr == nil {
		res.Stage, res.History = u.Spec.Stage, u.Status.History
	}
	return res, err
}

// resolveSeed finds the seed image o.Seed names, its OpenShift version, and
// the vSNO it was made from ("" for an image outside the registry).
func resolveSeed(ctx context.Context, e Env, o UpgradeOptions) (image, version, from string, err error) {
	imgs, err := seed.List(ctx, e, o.Namespace)
	if err != nil {
		return "", "", "", err
	}
	m := seed.Match(imgs, o.Seed)
	if len(m) == 0 {
		if strings.Contains(o.Seed, "/") && o.SeedVersion != "" {
			return o.Seed, o.SeedVersion, "", nil // an image elsewhere; its version given
		}
		return "", "", "", fmt.Errorf("no seed image %s in %s (see `vsno seed list`); for an image elsewhere, pass its reference and --seed-version", o.Seed, o.Namespace)
	}
	newest := slices.MaxFunc(m, func(a, b seed.Image) int { return a.Created.Compare(b.Created) })
	version = newest.Version
	if o.SeedVersion != "" {
		version = o.SeedVersion
	}
	if version == "" {
		return "", "", "", fmt.Errorf("seed image %s has no version label; pass --seed-version", newest.Ref)
	}
	return newest.Ref, version, newest.From, nil
}

// current reads the target's ImageBasedUpgrade, which LCA creates once it runs.
func current(ctx context.Context, a guestctl.Access) (IBU, error) {
	u, found, err := read(ctx, a)
	if err == nil && !found {
		err = errors.New("LCA has not created its ImageBasedUpgrade yet")
	}
	return u, err
}

// resumeFrom says which stage to start from for an upgrade to image: Prep
// from Idle, or the stage the target is in with the same seed.
func resumeFrom(u IBU, image string) (Stage, error) {
	switch u.Spec.Stage {
	case StageIdle, "":
		if !slices.Contains(u.Status.ValidNextStages, StagePrep) && len(u.Status.ValidNextStages) > 0 {
			return "", fmt.Errorf("LCA won't start Prep now (it allows %v)", u.Status.ValidNextStages)
		}
		return StagePrep, nil
	case StagePrep, StageUpgrade:
		if u.Spec.SeedImageRef.Image != image {
			return "", fmt.Errorf("the target is in %s with seed %s; finish or roll that back first", u.Spec.Stage, u.Spec.SeedImageRef.Image)
		}
		return u.Spec.Stage, nil
	}
	return "", fmt.Errorf("the target is in %s; wait for it to finish", u.Spec.Stage)
}

// checkSeed requires the vSNO the seed was made from, when it's here and
// answers, to match the target where LCA requires it: the same LCA version,
// and openshift-config/user-ca-bundle on both or neither. A vSNO installed
// from the release cache has that ConfigMap (the registry's CA) and one
// installed from the internet doesn't, so a seed and target must have been
// installed the same way.
func checkSeed(ctx context.Context, e Env, from, targetName string, target guestctl.Access, res *UpgradeResult) error {
	if from == "" || from == targetName {
		return nil
	}
	sa := e.Access(from)
	theirs, err := seed.InstalledLCA(ctx, sa)
	if err != nil || theirs == "" {
		res.Warnings = append(res.Warnings, "can't read the seed vSNO "+from+", so it isn't compared with the target (LCA version, trust bundle)")
		return nil //nolint:nilerr // a stopped seed vSNO is normal after seed generation
	}
	ours, err := seed.InstalledLCA(ctx, target)
	if err != nil {
		return err
	}
	if ours != theirs {
		return fmt.Errorf("the seed was made with %s and the target runs %s; LCA must be the same version on both", theirs, ours)
	}
	seedHas, err := hasUserCABundle(ctx, sa)
	if err != nil {
		return err
	}
	targetHas, err := hasUserCABundle(ctx, target)
	if err != nil {
		return err
	}
	if seedHas != targetHas {
		return fmt.Errorf("the target %s openshift-config/user-ca-bundle and the seed vSNO %s %s; LCA needs both or neither. "+
			"A vSNO installed from the release cache has it and one installed with --no-cache doesn't: make the seed from a vSNO installed the same way as the target",
			hasText(targetHas), from, hasText(seedHas))
	}
	return nil
}

// hasUserCABundle reports whether the vSNO has openshift-config/user-ca-bundle.
func hasUserCABundle(ctx context.Context, a guestctl.Access) (bool, error) {
	var cm guestctl.Object
	return guestctl.Get(ctx, a, &cm, "get", "configmap", "user-ca-bundle", "-n", "openshift-config", "--ignore-not-found")
}

func hasText(has bool) string {
	if has {
		return "has"
	}
	return "doesn't have"
}

// moveTo asks LCA for stage s, unless the target is already there (at), and
// follows it there. The API goes away while the node reboots, so a read that
// fails is "not yet", not a failure.
func moveTo(ctx context.Context, a guestctl.Access, c guestctl.Clock, at, s Stage, limit time.Duration, res *UpgradeResult, ps precheck.ProgressStep) (string, error) {
	start := c.Now()
	if at != s {
		if err := setStage(ctx, a, s, res); err != nil {
			return "", err
		}
	}
	var last string
	down := time.Time{}
	err := guestctl.Poll(ctx, c, limit, func() (bool, error) {
		u, found, err := read(ctx, a)
		if err != nil || !found {
			if down.IsZero() {
				down = c.Now()
			}
			why := "isn't answering"
			if s == StageUpgrade || s == StageRollback {
				why = "is down while the node reboots"
			}
			ps.Update(fmt.Sprintf("the vSNO's API %s (%s)", why, c.Now().Sub(down).Round(time.Second)))
			return false, nil
		}
		down = time.Time{}
		if s == StageUpgrade {
			if rb, msg := u.Toward(StageRollback); rb != Moving || u.Spec.Stage == StageRollback {
				return false, fmt.Errorf("LCA rolled the upgrade back: %s", msg)
			}
		}
		p, msg := u.Toward(s)
		last = msg
		if pulled, failed, total, ok := Precache(msg); ok {
			ps.SetProgress(pulled+failed, total)
		}
		ps.Update(msg)
		switch p {
		case Arrived:
			return true, nil
		case Stuck:
			return false, fmt.Errorf("%s failed: %s", s, msg)
		}
		return false, nil
	})
	if errors.Is(err, guestctl.ErrTimeout) {
		return "", fmt.Errorf("%s didn't finish in %s; last: %s", s, limit, last)
	}
	return fmt.Sprintf("in %s", c.Now().Sub(start).Round(time.Second)), err
}

// setStage patches the ImageBasedUpgrade to stage s; Prep also names the seed.
func setStage(ctx context.Context, a guestctl.Access, s Stage, res *UpgradeResult) error {
	spec := map[string]any{"stage": s}
	if s == StagePrep {
		spec["seedImageRef"] = map[string]any{
			"version": res.SeedVersion, "image": res.SeedImage, "pullSecretRef": map[string]string{"name": PullSecret},
		}
	}
	patch, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return err
	}
	return guestctl.Oc(ctx, a, "", "patch", "imagebasedupgrades", ibuName, "--type", "merge", "-p", string(patch))
}
