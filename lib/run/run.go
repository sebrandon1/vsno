// Package run does a whole Image Based Upgrade from a kubeconfig: it sets the
// environment up, creates a seed and a target vSNO, makes a seed image from
// the seed and upgrades the target with it. Each stage reuses what an earlier
// run left (a vSNO at the right version, the seed image, an upgrade part way),
// so running it again carries on where it stopped.
package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/ibu"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/release"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/snapshot"
	"github.com/sebrandon1/vsno/lib/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Stage is one part of a run.
type Stage string

// The stages, in order. Delete only runs with Options.Delete.
const (
	StageSetup   Stage = "setup"
	StageVSNOs   Stage = "vsnos"
	StageSeed    Stage = "seed"
	StageUpgrade Stage = "upgrade"
	StageDelete  Stage = "delete"
)

// StageName is what a run records in the environment's state, per stage:
// "run:<stage>".
const StageName = "run"

// Env is what a run needs: seed's environment (the guest package's, plus
// access to each vSNO), a setup to run, and the releases to pick versions from.
type Env struct {
	seed.Env
	// Setup prepares the environment, as vsno setup does.
	Setup func(ctx context.Context) error
	// Stable lists a minor's stable releases, oldest first.
	Stable func(ctx context.Context, minor string) ([]string, error)
	// Confirm asks the user to type answer to approve question; nil never approves.
	Confirm func(question, answer string) bool
}

// Options are the user's choices; empty fields get the defaults below.
type Options struct {
	Namespace string `json:"namespace,omitempty"`
	// TargetVersion and SeedVersion are what vsno create accepts. By default
	// the seed is the newest stable release of the host's minor and the
	// target the one before it.
	TargetVersion string `json:"targetVersion,omitempty"`
	SeedVersion   string `json:"seedVersion,omitempty"`
	TargetName    string `json:"targetName,omitempty"` // default "target"
	SeedName      string `json:"seedName,omitempty"`   // default "seed"
	// SeedImage upgrades to an existing seed image: no seed vSNO is made.
	// Its version comes from its labels, or SeedVersion.
	SeedImage string `json:"seedImage,omitempty"`
	// SeedRepo pushes the seed image to <repo>:<version> instead of the
	// environment's registry.
	SeedRepo string        `json:"seedRepo,omitempty"`
	LCA      ibu.LCASource `json:"lca,omitzero"`
	Node     string        `json:"node,omitempty"`
	// Disk is the disk setup gives LVMS on Node, as vsno setup --disk.
	Disk      string `json:"disk,omitempty"`
	Cores     int    `json:"cores,omitempty"`
	MemoryGiB int64  `json:"memoryGiB,omitempty"`
	// Finalize, Rollback and Delete are what to do after the upgrade.
	Finalize bool `json:"finalize,omitempty"`
	Rollback bool `json:"rollback,omitempty"`
	Delete   bool `json:"delete,omitempty"`
	// Yes approves the plan without asking.
	Yes bool `json:"yes,omitempty"`
	// Restart runs every stage again instead of skipping the ones an earlier
	// run of the same plan finished. Each stage still reuses what is on the
	// cluster, so little is rebuilt.
	Restart bool `json:"restart,omitempty"`
}

// Plan is what a run will do, shown before it starts.
type Plan struct {
	Namespace     string  `json:"namespace"`
	Target        string  `json:"target"`
	TargetVersion string  `json:"targetVersion"`
	Seed          string  `json:"seed,omitempty"` // "" with a seed image
	SeedVersion   string  `json:"seedVersion"`
	SeedImage     string  `json:"seedImage,omitempty"`
	LCA           string  `json:"lca"`
	Then          string  `json:"then"` // finalize, rollback, delete, or keep
	Stages        []Stage `json:"stages"`
	Done          []Stage `json:"done,omitempty"` // stages an earlier run finished
	// Restore is the snapshot each vSNO the plan creates is restored from
	// instead of installed, by vSNO name. A vSNO keeps its snapshot's name and
	// version, so only a snapshot at the plan's version is used.
	Restore map[string]string `json:"restore,omitempty"`
	// Restart is set when this run ignores Done and runs every stage.
	Restart bool      `json:"restart,omitempty"`
	At      time.Time `json:"-"`
}

// Result is a finished, or failed, run.
type Result struct {
	Plan    Plan             `json:"plan"`
	Timings map[Stage]string `json:"timings"`
	// Failed is the stage the run stopped at, if one failed.
	Failed  Stage              `json:"failed,omitempty"`
	Upgrade *ibu.UpgradeResult `json:"upgrade,omitempty"`
}

func (o *Options) defaults() {
	if o.TargetName == "" {
		o.TargetName = "target"
	}
	if o.SeedName == "" {
		o.SeedName = "seed"
	}
	// The sizes are known before any create, for the capacity check.
	sized := guest.CreateOptions{Cores: o.Cores, MemoryGiB: o.MemoryGiB}.Sized()
	o.Cores, o.MemoryGiB = sized.Cores, sized.MemoryGiB
}

// After is what a run does once the upgrade completed. The options name it
// with one of Finalize, Rollback and Delete, which check keeps exclusive.
type After string

const (
	AfterKeep     After = "keep" // leave the target in UpgradeCompleted
	AfterFinalize After = "finalize"
	AfterRollback After = "rollback"
	AfterDelete   After = "delete"
)

// after is the one of Finalize, Rollback and Delete o sets, or AfterKeep.
func (o Options) after() After {
	switch {
	case o.Finalize:
		return AfterFinalize
	case o.Rollback:
		return AfterRollback
	case o.Delete:
		return AfterDelete
	}
	return AfterKeep
}

// afterText is each After as the plan shows it.
var afterText = map[After]string{
	AfterKeep:     "leave the target in UpgradeCompleted",
	AfterFinalize: "finalize the upgrade",
	AfterRollback: "roll the upgrade back",
	AfterDelete:   "delete both vSNOs",
}

func (o Options) check() error {
	switch {
	case o.Finalize && o.Rollback:
		return errors.New("--finalize and --rollback are opposites; pick one")
	case o.Delete && (o.Finalize || o.Rollback):
		return errors.New("--delete removes both vSNOs, so --finalize and --rollback would be undone; pick one")
	case o.SeedImage == "" && o.TargetName == o.SeedName:
		return fmt.Errorf("the seed and the target are both %s; name them apart", o.TargetName)
	}
	return nil
}

// Run plans the run, asks unless o.Yes, then runs each stage not yet done.
func Run(ctx context.Context, e Env, o Options) (*Result, error) {
	e.Defaults(o.Namespace)
	o.defaults()
	if err := o.check(); err != nil {
		return nil, err
	}
	plan, err := makePlan(ctx, e, o)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: *plan, Timings: map[Stage]string{}}
	if !o.Yes && (e.Confirm == nil || !e.Confirm(plan.String()+"\nRun it? Type y", "y")) {
		return res, errors.New("not confirmed; pass --yes to run without asking")
	}
	for _, s := range plan.Stages {
		if slices.Contains(plan.Done, s) {
			continue // an earlier run of this plan finished it
		}
		start := e.Now()
		err := runStage(ctx, e, o, plan, s, res)
		res.Timings[s] = e.Now().Sub(start).Round(time.Second).String()
		record(ctx, e, o.Namespace, plan.key(), s, err)
		if err != nil {
			res.Failed = s
			return res, fmt.Errorf("%s: %w", s, err)
		}
	}
	return res, nil
}

func runStage(ctx context.Context, e Env, o Options, plan *Plan, s Stage, res *Result) error {
	switch s {
	case StageSetup:
		return e.Setup(ctx)
	case StageVSNOs:
		// One listing for both: it probes every vSNO in the namespace.
		existing, err := guest.List(ctx, e.Env.Env, o.Namespace)
		if err != nil {
			return err
		}
		names, err := toCreate(ctx, e, o, plan, existing)
		if err != nil {
			return err
		}
		if err := mirror(ctx, e, o, plan, names, existing); err != nil {
			return err
		}
		return ensureVSNOs(ctx, e, o, plan, names, existing)
	case StageSeed:
		return ensureSeed(ctx, e, o, plan)
	case StageUpgrade:
		// The target, if the node had no room for it next to the seed.
		existing, err := guest.List(ctx, e.Env.Env, o.Namespace)
		if err != nil {
			return err
		}
		if err := mirror(ctx, e, o, plan, []string{plan.Target}, existing); err != nil {
			return err
		}
		if err := ensureVSNOs(ctx, e, o, plan, []string{plan.Target}, existing); err != nil {
			return err
		}
		u, err := ibu.Upgrade(ctx, e.Env, ibu.UpgradeOptions{Namespace: o.Namespace, Target: plan.Target,
			Seed: plan.SeedImage, SeedVersion: plan.SeedVersion, Finalize: o.after() == AfterFinalize, Rollback: o.after() == AfterRollback, LCA: o.LCA})
		res.Upgrade = u
		return err
	case StageDelete:
		names := []string{plan.Target}
		if plan.Seed != "" {
			names = append(names, plan.Seed)
		}
		// The plan, which named them, was already approved.
		_, err := guest.Delete(ctx, e.Env.Env, guest.DeleteOptions{Namespace: o.Namespace, Names: names, Yes: true})
		return err
	}
	return fmt.Errorf("unknown stage %s", s)
}

// makePlan resolves the versions and names, and notes what an earlier run
// already finished.
func makePlan(ctx context.Context, e Env, o Options) (*Plan, error) {
	p := &Plan{Namespace: o.Namespace, Target: o.TargetName, TargetVersion: o.TargetVersion, SeedVersion: o.SeedVersion,
		SeedImage: o.SeedImage, LCA: lcaText(o.LCA), Then: afterText[o.after()], Restart: o.Restart, At: e.Now()}
	if o.SeedImage == "" {
		p.Seed = o.SeedName
	}
	if err := p.versions(ctx, e, o); err != nil {
		return nil, err
	}
	if p.SeedVersion != "" && release.Compare(p.TargetVersion, p.SeedVersion) >= 0 {
		return nil, fmt.Errorf("the target (%s) must be older than the seed (%s)", p.TargetVersion, p.SeedVersion)
	}
	p.Stages = []Stage{StageSetup, StageVSNOs}
	if o.SeedImage == "" {
		p.Stages = append(p.Stages, StageSeed)
	}
	p.Stages = append(p.Stages, StageUpgrade)
	if o.after() == AfterDelete {
		p.Stages = append(p.Stages, StageDelete)
	}
	if !o.Restart {
		p.Done = finished(ctx, e, o.Namespace, p.key())
	}
	restore, err := restores(ctx, e, o, p.wanted())
	if err != nil {
		return nil, err
	}
	if len(restore) > 0 {
		p.Restore = restore
	}
	return p, nil
}

// versions fills in the releases the plan leaves unset with the host minor's
// last two stable ones: the newest is the seed, the one before it the target.
// A seed image brings its own version, so only a seed vSNO needs one.
func (p *Plan) versions(ctx context.Context, e Env, o Options) error {
	if p.TargetVersion != "" && (p.SeedVersion != "" || o.SeedImage != "") {
		return nil
	}
	target, seedV, err := defaultVersions(ctx, e)
	if err != nil {
		return err
	}
	if p.TargetVersion == "" {
		p.TargetVersion = target
	}
	if p.SeedVersion == "" && o.SeedImage == "" {
		p.SeedVersion = seedV
	}
	return nil
}

// restores is the snapshot each wanted vSNO is restored from: the snapshot of
// that name, at the version the plan asks for, when the vSNO isn't there yet.
// A restore brings the disk, so it costs a minute instead of an install. A
// snapshot is only usable under its own name and version, and a vSNO that
// exists is left as it is (matches refuses one at another version).
func restores(ctx context.Context, e Env, o Options, want map[string]string) (map[string]string, error) {
	out := map[string]string{}
	se := snapshot.Env{Env: e.Env.Env}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		_, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(o.Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			continue
		case !apierrors.IsNotFound(err):
			return nil, fmt.Errorf("checking for vSNO %s: %w", name, err)
		}
		ent, err := snapshot.Use(ctx, se, o.Namespace, name)
		if err != nil {
			return nil, err
		}
		if ent != nil && ent.Version == want[name] {
			out[name] = ent.Name
		}
	}
	return out, nil
}

// defaultVersions are the newest stable release of the host's minor (the
// seed) and the one before it (the target).
func defaultVersions(ctx context.Context, e Env) (target, seedVersion string, err error) {
	cv, err := e.Dyn.Resource(precheck.GVRClusterVersion).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("reading the host's version to pick defaults (pass --target-version and --seed-version): %w", err)
	}
	v, _, _ := unstructured.NestedString(cv.Object, "status", "desired", "version")
	minor := release.Minor(v)
	vs, err := e.Stable(ctx, minor)
	if err != nil {
		return "", "", err
	}
	if len(vs) < 2 {
		return "", "", fmt.Errorf("stable-%s has %d release(s); an upgrade needs two (pass --target-version and --seed-version)", minor, len(vs))
	}
	return vs[len(vs)-2], vs[len(vs)-1], nil
}

// wanted is each vSNO the plan names, with its version.
func (p Plan) wanted() map[string]string {
	w := map[string]string{p.Target: p.TargetVersion}
	if p.Seed != "" {
		w[p.Seed] = p.SeedVersion
	}
	return w
}

// toCreate is the vSNOs the vSNOs stage makes ready: both, unless neither
// exists yet and the node has room for only one. Then it's the seed alone,
// which seed generation stops, and the upgrade stage creates the target.
func toCreate(ctx context.Context, e Env, o Options, plan *Plan, existing []guest.Guest) ([]string, error) {
	all := slices.Sorted(maps.Keys(plan.wanted()))
	if plan.Seed == "" {
		return all, nil
	}
	for _, g := range existing {
		if g.Name == plan.Seed || g.Name == plan.Target {
			return all, nil // one is there: creating the other checks its own room
		}
	}
	node := o.Node
	if node == "" {
		st, _, err := state.Load(ctx, e.Kube, o.Namespace)
		if err != nil {
			return nil, err
		}
		node = st.Node
	}
	err := guest.Fit(ctx, e.Env.Env, node, o.Cores, o.MemoryGiB, 2)
	switch {
	case errors.Is(err, guest.ErrNoCapacity):
		e.Progress.Start("Room for both vSNOs").Done("no: " + err.Error() + "; creating " + plan.Seed + " first, and " + plan.Target + " once the seed is made and " + plan.Seed + " stopped")
		return []string{plan.Seed}, nil
	case err != nil:
		return nil, err
	}
	return all, nil
}

// ensureVSNOs creates the named vSNOs unless they exist, starting all before
// waiting for any, and waits until they are ready.
func ensureVSNOs(ctx context.Context, e Env, o Options, plan *Plan, names []string, existing []guest.Guest) error {
	want := plan.wanted()
	for _, name := range names {
		if i := slices.IndexFunc(existing, func(g guest.Guest) bool { return g.Name == name }); i >= 0 {
			if err := matches(existing[i], want[name]); err != nil {
				return err
			}
			// An earlier stage may have stopped it: seedgen does, and a
			// resumed run or a stopped vSNO would otherwise wait forever.
			if existing[i].State == guest.Stopped {
				if err := guest.SetRunning(ctx, e.Env.Env, o.Namespace, name, true); err != nil {
					return fmt.Errorf("starting %s: %w", name, err)
				}
			}
			continue
		}
		if _, err := guest.Create(ctx, e.Env.Env, guest.CreateOptions{Namespace: o.Namespace, Name: name, Version: want[name],
			Node: o.Node, Cores: o.Cores, MemoryGiB: o.MemoryGiB, Snapshot: plan.Restore[name]}); err != nil {
			return fmt.Errorf("creating %s: %w", name, err)
		}
	}
	for _, name := range names {
		if st, err := guest.WaitState(ctx, e.Env.Env, o.Namespace, name, guest.Ready); err != nil {
			return fmt.Errorf("waiting for %s (%s): %w", name, st, err)
		}
	}
	return nil
}

// mirror mirrors the releases the vSNOs about to be created install from, all
// at once: on a cold cache each mirror is about 8 minutes, and Create's own
// mirrors are one after the other. A vSNO that already exists or is restored
// from a snapshot needs nothing, and Create then finds its release mirrored.
func mirror(ctx context.Context, e Env, o Options, plan *Plan, names []string, existing []guest.Guest) error {
	if e.Cache == nil {
		return nil
	}
	want := plan.wanted()
	var rels []release.Release
	for _, name := range names {
		if plan.Restore[name] != "" || slices.ContainsFunc(existing, func(g guest.Guest) bool { return g.Name == name }) {
			continue
		}
		rel, err := e.Resolver.Resolve(ctx, want[name])
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(rels, func(r release.Release) bool { return r.Image == rel.Image }) {
			rels = append(rels, rel)
		}
	}
	if len(rels) == 0 {
		return nil
	}
	versions := make([]string, len(rels))
	for i, rel := range rels {
		versions[i] = rel.Requested
	}
	ps := e.Progress.Start("Mirroring " + strings.Join(versions, ", "))
	// One mirror failing cancels the others: a run without both is going to
	// stop anyway, and a half-mirrored registry is worse than none.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(rels))
	for i, rel := range rels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			onLine := func(line string) { ps.Update(rel.Requested + ": " + line) }
			if _, err := e.Cache.Ensure(ctx, o.Namespace, rel, onLine); err != nil {
				errs[i] = err
				cancel()
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		ps.Fail(err.Error())
		return err
	}
	ps.Done(fmt.Sprintf("%d release(s)", len(rels)))
	return nil
}

// matches refuses a vSNO of that name at another version: it isn't the one
// the plan asked for, and upgrading or seeding from it would mislead.
func matches(g guest.Guest, version string) error {
	have := g.ShownVersion()
	if have == "" || version == "" || have == version || strings.HasPrefix(have, version+".") || g.Requested == version {
		return nil
	}
	return fmt.Errorf("vSNO %s already exists at %s, not %s; delete it or pass another name", g.Name, have, version)
}

// ensureSeed makes the seed image from the seed vSNO unless it exists, and
// sets plan.SeedImage to it.
func ensureSeed(ctx context.Context, e Env, o Options, plan *Plan) error {
	image := ""
	if o.SeedRepo != "" {
		image = o.SeedRepo + ":" + plan.SeedVersion
	} else if imgs, err := seed.List(ctx, e.Env, o.Namespace); err == nil {
		for _, img := range seed.Match(imgs, plan.Seed+":"+plan.SeedVersion) {
			plan.SeedImage = img.Ref // made by an earlier run
			return nil
		}
	}
	// seed create needs LCA and the registry's trust on the seed vSNO.
	if _, err := ibu.Prepare(ctx, e.Env, ibu.PrepareOptions{Namespace: o.Namespace, Names: []string{plan.Seed}, Role: ibu.RoleSeed, LCA: o.LCA}); err != nil {
		return err
	}
	r, err := seed.Create(ctx, e.Env, seed.CreateOptions{Namespace: o.Namespace, Name: plan.Seed, Image: image, Stop: true})
	if err != nil {
		return err
	}
	plan.SeedImage = r.Image
	return nil
}

// record notes a stage's outcome in the environment's state, best effort:
// the run's own output is what the user reads.
// record notes a stage's outcome in the environment's state; a finished
// stage's detail is the plan's key, so only a run of the same plan resumes it.
func record(ctx context.Context, e Env, ns, key string, s Stage, err error) {
	st, _, lerr := state.Load(ctx, e.Kube, ns)
	if lerr != nil {
		return
	}
	status, detail := state.Done, key
	if err != nil {
		status, detail = state.Failed, err.Error()
	}
	st.Record(StageName+":"+string(s), status, detail, e.Now())
	_ = state.Save(ctx, e.Kube, ns, st)
}

// finished lists the stages the environment's state says a run of the plan
// with this key finished, most recent outcome per stage.
func finished(ctx context.Context, e Env, ns, key string) []Stage {
	st, _, err := state.Load(ctx, e.Kube, ns)
	if err != nil {
		return nil
	}
	var out []Stage
	for _, s := range []Stage{StageSetup, StageVSNOs, StageSeed, StageUpgrade, StageDelete} {
		name := StageName + ":" + string(s)
		// A stage this plan finished once stays finished, even if a later
		// attempt at it failed: a run that re-ran finished stages (before Run
		// skipped them) could fail at an upgrade that had already succeeded,
		// and that failure would otherwise block the plan's delete stage
		// forever.
		for _, rec := range st.Stages {
			if rec.Name == name && rec.Status == state.Done && rec.Detail == key {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// key names what the plan upgrades and with what, e.g.
// "target@4.22.21 from seed@4.22.22": stages a run of another plan finished
// aren't this one's to resume.
func (p Plan) key() string {
	from := p.Seed
	if from == "" {
		from = p.SeedImage
	}
	return p.Target + "@" + p.TargetVersion + " from " + from + "@" + p.SeedVersion
}

// String is the plan as the confirmation shows it.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for %s:\n", p.Namespace)
	if p.Seed != "" {
		fmt.Fprintf(&b, "  seed     vSNO %s at %s, %s\n", p.Seed, p.SeedVersion, vSNOText(p.Restore, p.Seed, "made into a seed image"))
	} else {
		fmt.Fprintf(&b, "  seed     image %s\n", p.SeedImage)
	}
	fmt.Fprintf(&b, "  target   vSNO %s at %s, %s\n", p.Target, p.TargetVersion,
		vSNOText(p.Restore, p.Target, "upgraded to the seed's version"))
	fmt.Fprintf(&b, "  LCA      %s\n", p.LCA)
	fmt.Fprintf(&b, "  then     %s\n", p.Then)
	if len(p.Done) > 0 {
		fmt.Fprintf(&b, "  resuming: an earlier run finished %v\n", p.Done)
	}
	if p.Restart {
		fmt.Fprintf(&b, "  restarting: every stage runs again, reusing what's there\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// vSNOText says how a vSNO gets there and what the plan then does with it:
// "restored from snapshot target, upgraded to the seed's version" when a
// snapshot brings the disk, and just the second half when it is installed.
func vSNOText(restore map[string]string, name, then string) string {
	if s := restore[name]; s != "" {
		return "restored from snapshot " + s + ", " + then
	}
	return then
}

func lcaText(s ibu.LCASource) string {
	switch {
	case s.FromCatalog():
		return "each vSNO's catalog"
	case s.Image != "":
		return "image " + s.Image
	}
	return "built from " + s.Ref
}
