package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/release"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage the OpenShift releases mirrored into the environment's registry",
	Long: `cache lists the OpenShift releases vsno has mirrored into the environment's
registry, which vSNOs then install from instead of pulling 15-20 GB from the
internet. create mirrors a release the first time it is used; --no-cache turns
that off.`,
}

var cacheAddCmd = &cobra.Command{
	Use:   "add <version>",
	Short: "Mirror an OpenShift release into the environment's registry",
	Long: `add mirrors a release's payload into the environment's registry, so vSNOs of
that version install from the cluster instead of the internet. create does
this itself the first time it uses a version; add is for doing it ahead of
time, or for a version you want ready.

It returns as soon as the mirror is running: the work happens in a Job in the
environment, so it carries on after the command exits. --watch follows it to
the end instead, and vsno cache list shows how far it has got. A release
already cached comes back at once.

<version> takes what create accepts: 4.20.46, 4.20, a nightly, or a release
image pullspec. Nightlies need registry.ci.openshift.org credentials in the
cluster's pull secret, which create already requires for them.`,
	Args: oneArg("the release to mirror: 4.20.46, 4.20, a nightly, or a release image pullspec"),
	RunE: runCacheAdd,
}

var cachePruneCmd = &cobra.Command{
	Use:   "prune [<version>]",
	Short: "Remove cached releases, and collect the registry's garbage",
	Long: `prune removes a cached release, or the ones no vSNO uses (--keep N keeps the
N most recently used). It deletes their images from the registry and then runs
the registry's garbage collector, which is what frees the space; the registry
is unavailable while that runs.

It lists what it would remove and asks you to type "prune" first, unless
--yes. --dry-run only lists. A release a vSNO still runs is refused.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCachePrune,
}

var cacheGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Free the space of deleted images by collecting the registry's garbage",
	Long: `gc runs the registry's own garbage collector over its volume. It is what frees
the space that deleted images still hold, whether they were removed by prune or
by "vsno seed delete".

The registry is unavailable while it runs, so this stops it, runs the collector
in a Job, and starts it again; it refuses while a release mirror is running.

--dry-run says what it would free — the registry volume's used bytes less what
the tagged releases and seed images still take — and changes nothing.`,
	Args: cobra.NoArgs,
	RunE: runCacheGC,
}

var cacheListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the mirrored releases, their size, and the vSNOs using them",
	Long: `list shows each cached release, its size and the vSNOs installing from it.
A release being mirrored is shown as mirroring, with how much it has pushed,
how fast and how much is left; -w refreshes until interrupted. A mirror that
finished while nothing was watching is recorded before the list is shown.`,
	Args: cobra.NoArgs,
	RunE: runCacheList,
}

var (
	cgYes      bool
	cgDryRun   bool
	cpKeep     int
	cpYes      bool
	cpDryRun   bool
	cpNoGC     bool
	caWatch    bool
	cwWatch    bool
	cwInterval time.Duration
)

func init() {
	f := cachePruneCmd.Flags()
	f.IntVar(&cpKeep, "keep", 0, "keep the N most recently used unused releases")
	f.BoolVar(&cpYes, "yes", false, "don't ask for confirmation")
	f.BoolVar(&cpDryRun, "dry-run", false, "list what would be removed and change nothing")
	f.BoolVar(&cpNoGC, "no-gc", false, "skip the registry garbage collection, which is what frees the space")
	cacheAddCmd.Flags().BoolVarP(&caWatch, "watch", "w", false, "follow the mirror until it finishes")
	cacheListCmd.Flags().BoolVarP(&cwWatch, "watch", "w", false, "refresh until interrupted")
	cacheListCmd.Flags().DurationVar(&cwInterval, "interval", 15*time.Second, "refresh interval for --watch")
	cacheGCCmd.Flags().BoolVar(&cgYes, "yes", false, "don't ask for confirmation")
	cacheGCCmd.Flags().BoolVar(&cgDryRun, "dry-run", false, "report what the collection would free and change nothing")
	cacheCmd.AddCommand(cacheAddCmd, cacheListCmd, cacheGCCmd, cachePruneCmd)
	rootCmd.AddCommand(cacheCmd)
}

// cacheEnv is the cache and the vSNOs, which say which release each uses.
func cacheEnv(cmd *cobra.Command) (cache.Env, guest.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return cache.Env{}, guest.Env{}, err
	}
	if err := useEnvironment(cmd.Context(), c.Kube); err != nil {
		return cache.Env{}, guest.Env{}, err
	}
	genv, err := guestEnv(cmd.Context(), c, 0)
	if err != nil {
		return cache.Env{}, guest.Env{}, err
	}
	env := cache.Env{Kube: c.Kube, Dyn: c.Dyn, Config: c.Config, Builder: genv.Builder}
	return env, genv, nil
}

func runCacheAdd(cmd *cobra.Command, args []string) error {
	env, _, err := cacheEnv(cmd)
	if err != nil {
		return err
	}
	rel, err := release.Resolver{}.Resolve(cmd.Context(), args[0])
	if err != nil {
		return fmt.Errorf("%w; `vsno cache list` shows what is already mirrored", err)
	}
	o := cache.EnsureOptions{Namespace: namespace, Release: rel}
	if !caWatch {
		// Start is quick for a release that is not cached, but a hit re-checks
		// the mirror's signatures: one listing of its tags per image.
		ps := newProgress().Start("Checking " + args[0])
		p, err := cache.Start(cmd.Context(), env, o)
		if err != nil {
			ps.Fail(err.Error())
			return err
		}
		ps.Done("")
		return reportCacheAdd(cmd, p, args[0], nil)
	}
	ps := newProgress().Start("Mirroring " + args[0])
	o.OnLine = ps.Update
	p, err := cache.Ensure(cmd.Context(), env, o)
	if err != nil {
		ps.Fail(err.Error())
		return err
	}
	return reportCacheAdd(cmd, p, args[0], ps)
}

// reportCacheAdd says what happened: the release was already there, a mirror
// is running, or --watch followed one to the end.
func reportCacheAdd(cmd *cobra.Command, p *cache.Prepared, asked string, ps *ui.Step) error {
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if ps != nil {
			ps.Done("")
		}
		return writeJSON(w, p)
	}
	done := func(msg string) {
		if ps != nil {
			ps.Done(msg)
		}
	}
	switch {
	case p.Hit:
		done("already cached")
		fmt.Fprintf(w, "%s %s is already in %s.\n", pal.Icon("✓"), p.Image, p.Repo)
	case p.Running && ps == nil && p.Followed:
		fmt.Fprintf(w, "%s %s is already being mirrored into %s as Job %s.\n", pal.Icon("→"), p.Image, p.Repo, p.Job)
		fmt.Fprintf(w, "  %s\n", pal.Dim("vsno cache list                    how far it has got"))
		fmt.Fprintf(w, "  %s\n", pal.Dim("vsno cache add "+asked+" --watch   follow it here"))
	case p.Running && ps == nil:
		fmt.Fprintf(w, "%s Mirroring %s into %s as Job %s, in the background.\n", pal.Icon("→"), p.Image, p.Repo, p.Job)
		fmt.Fprintf(w, "  %s\n", pal.Dim("vsno cache list                    how far it has got"))
		fmt.Fprintf(w, "  %s\n", pal.Dim("vsno cache add "+asked+" --watch   follow it here"))
	default:
		done("mirrored in " + ui.FormatElapsed(p.Elapsed))
		fmt.Fprintf(w, "%s Mirrored %s into %s: %s in %s (%s).\n", pal.Icon("✓"), p.Image, p.Repo,
			bytesText(p.SizeBytes), ui.FormatElapsed(p.Elapsed), cache.Rate(p.SizeBytes, p.Elapsed))
		if p.Signatures != "" {
			fmt.Fprintf(w, "  %s\n", pal.Dim("signatures: "+p.Signatures))
		}
		fmt.Fprintln(w, "vSNOs of this version now install from the registry.")
	}
	return nil
}

func runCachePrune(cmd *cobra.Command, args []string) error {
	env, genv, err := cacheEnv(cmd)
	if err != nil {
		return err
	}
	users, err := releaseUsers(cmd, genv)
	if err != nil {
		return err
	}
	o := cache.PruneOptions{Namespace: namespace, Keep: cpKeep, Users: users,
		DryRun: cpDryRun, Yes: cpYes, NoGC: cpNoGC, Confirm: confirmOnTerminal(os.Stdin, cmd.ErrOrStderr())}
	if len(args) == 1 {
		o.Version = args[0]
	}
	ew := cmd.ErrOrStderr()
	shown := false
	o.ShowPlan = func(p *cache.PrunePlan) {
		renderCachePlan(ew, p, palette(ew))
		fmt.Fprintln(ew)
		shown = true
	}
	res, err := cache.Prune(cmd.Context(), env, o)
	if err != nil { // a refused prune has no result to render
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if rerr := writeJSON(w, res); rerr != nil && err == nil {
			err = rerr
		}
		return err
	}
	switch {
	case len(res.Removed) > 0:
		fmt.Fprintf(w, "%s Removed %d cached release(s)", pal.Icon("✓"), len(res.Removed))
		if res.Freed > 0 {
			fmt.Fprintf(w, ", freeing %s", bytesText(res.Freed))
		}
		fmt.Fprintln(w)
	case !shown && !res.Plan.Empty():
		renderCachePlan(w, &res.Plan, pal)
		if cpDryRun {
			fmt.Fprintln(w, "\nDry run: nothing was changed.")
		}
	case res.Plan.Empty():
		fmt.Fprintln(w, "Nothing to prune: every cached release is in use or within --keep.")
	}
	return err
}

func runCacheGC(cmd *cobra.Command, _ []string) error {
	env, genv, err := cacheEnv(cmd)
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if cgDryRun {
		plan, err := gcPlanFor(cmd.Context(), env, genv)
		if err != nil {
			return err
		}
		return renderGCDryRun(w, plan, pal)
	}
	if !cgYes {
		q := fmt.Sprintf("This stops the registry in %s for a few seconds to collect its garbage. Type %q to continue", namespace, "y")
		if !confirmOnTerminal(os.Stdin, cmd.ErrOrStderr())(q, "y|gc") {
			return errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	start := time.Now()
	ps := newProgress().Start("Collecting the registry's garbage")
	before, total, berr := env.Volume(cmd.Context(), namespace)
	if err := cache.GarbageCollect(cmd.Context(), env, namespace); err != nil {
		ps.Fail(err.Error())
		return err
	}
	after, _, aerr := env.Volume(cmd.Context(), namespace)
	elapsed := ui.FormatElapsed(time.Since(start))
	ps.Done("done in " + elapsed)
	res := gcReport{Used: before, Total: total}
	if berr == nil && aerr == nil && before > after { // an unread volume is not a zero
		res.Freed = before - after
	}
	if outputFormat == formatJSON {
		return cliout.PrintJSON(w, res)
	}
	freed := "nothing was left to free"
	if res.Freed > 0 {
		freed = "freeing " + bytesText(res.Freed)
	}
	fmt.Fprintf(w, "%s Collected the registry's garbage in %s, %s.\n", pal.Icon("✓"), elapsed, freed)
	return nil
}

// gcReport is what a garbage collection freed, or would free.
type gcReport struct {
	Used     int64 `json:"used"`               // the registry volume's bytes in use
	Total    int64 `json:"total"`              // the volume's size
	Tagged   int64 `json:"tagged,omitempty"`   // what the releases and seed images still take
	Freeable int64 `json:"freeable,omitempty"` // a dry run's estimate of what the collector can free
	Freed    int64 `json:"freed,omitempty"`    // what it did free
	DryRun   bool  `json:"dryRun,omitempty"`
}

// gcPlanFor measures what a collection would free: the volume's used bytes less
// the bytes the images vsno still has tagged take. Both are read while the
// registry stays up, so nothing is changed and nothing is stopped.
func gcPlanFor(ctx context.Context, env cache.Env, genv guest.Env) (gcReport, error) {
	// cache.List reads the volume too, so this is one df, not two.
	rel, err := cache.List(ctx, env, namespace, nil)
	if err != nil {
		return gcReport{}, err
	}
	rep := gcReport{Used: rel.Used, Total: rel.Total, DryRun: true}
	for _, e := range rel.Entries {
		rep.Tagged += e.SizeBytes
	}
	imgs, err := seed.List(ctx, seed.Env{Env: genv}, namespace)
	if err != nil {
		return rep, err
	}
	for _, i := range imgs {
		rep.Tagged += i.SizeBytes
	}
	if rep.Used > rep.Tagged {
		rep.Freeable = rep.Used - rep.Tagged
	}
	return rep, nil
}

// renderGCDryRun says what the collection would free, and that it did nothing.
func renderGCDryRun(w io.Writer, r gcReport, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return cliout.PrintJSON(w, r)
	}
	fmt.Fprintf(w, "The registry's volume holds %s of %s.\n", bytesText(r.Used), bytesText(r.Total))
	switch {
	case r.Freeable > 0:
		fmt.Fprintf(w, "Tagged releases and seed images take %s, so the collector would free about %s.\n",
			bytesText(r.Tagged), bytesText(r.Freeable))
	case r.Tagged <= r.Used:
		fmt.Fprintln(w, "The releases and seed images vsno has tagged account for it, so there is nothing to collect.")
	default:
		// Images share layers, and each image's size counts them, so the sum
		// can exceed the volume. Saying "nothing to free" would be a guess.
		fmt.Fprintf(w, "The tagged images add up to %s, more than the volume: they share layers, so this "+
			"measurement cannot say what a collection would free. The collector frees whatever no tag "+
			"references, which is usually little.\n", bytesText(r.Tagged))
	}
	fmt.Fprintln(w, pal.Dim("\nDry run: the registry was not stopped and nothing was changed."))
	return nil
}

func renderCachePlan(w io.Writer, p *cache.PrunePlan, pal ui.Palette) {
	for _, e := range p.Entries {
		fmt.Fprintf(w, "  %s %s  %s  %s\n", pal.Red("remove"), orDash(e.Version), e.Repo, pal.Dim(bytesText(e.SizeBytes)))
	}
	fmt.Fprintf(w, "\n%s\n", pal.Dim("Frees about "+bytesText(p.Bytes)+" once the registry's garbage is collected."))
}

// releaseUsers maps a release image to the vSNOs running it from the mirror.
func releaseUsers(cmd *cobra.Command, genv guest.Env) (map[string][]string, error) {
	gs, err := guest.List(cmd.Context(), genv, namespace)
	if err != nil {
		return nil, err
	}
	return cacheUsers(gs), nil
}

// cacheUsers maps a release image to the vSNOs that installed it from the
// mirror: a --no-cache guest runs the same image but does not need the cache.
func cacheUsers(gs []guest.Guest) map[string][]string {
	users := map[string][]string{}
	for _, g := range gs {
		if g.Cached && g.Image != "" {
			users[g.Image] = append(users[g.Image], g.Name)
		}
	}
	return users
}

func runCacheList(cmd *cobra.Command, _ []string) error {
	env, genv, err := cacheEnv(cmd)
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	for {
		// A mirror that ended while nothing was watching is recorded first, so
		// the list says what happened instead of showing it still running.
		if err := cache.Reconcile(cmd.Context(), env, namespace); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "note: "+err.Error())
		}
		users, err := releaseUsers(cmd, genv)
		if err != nil {
			return err
		}
		rep, err := cache.List(cmd.Context(), env, namespace, users)
		if err != nil {
			return err
		}
		if cwWatch && pal.On {
			fmt.Fprint(w, "\033[H\033[2J") // clear the screen between refreshes
		}
		if err := renderCache(w, rep, time.Now(), pal); err != nil || !cwWatch || outputFormat == formatJSON {
			return err
		}
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(cwInterval):
		}
	}
}

func renderCache(w io.Writer, rep *cache.Report, now time.Time, pal ui.Palette) error {
	if outputFormat == formatJSON {
		if rep.Entries == nil {
			rep.Entries = []cache.Entry{}
		}
		return writeJSON(w, rep)
	}
	if len(rep.Entries) == 0 {
		fmt.Fprintf(w, "No releases cached in %s. `vsno create` mirrors one the first time it needs it.\n", namespace)
		return nil
	}
	var t ui.Table
	t.Row(pal.Bold("VERSION"), pal.Bold("STATE"), pal.Bold("PROGRESS"), pal.Bold("IMAGE"), pal.Bold("SIZE"), pal.Bold("MIRRORED"), pal.Bold("LAST USED"), pal.Bold("VSNOS"))
	for _, e := range rep.Entries {
		t.Row(orDash(e.Version), e.StateText(), progressText(e), e.Image, bytesText(e.SizeBytes),
			ageSince(now, e.Mirrored), ageSince(now, e.LastUsed), joinItems(e.Users))
	}
	t.Write(w)
	if rep.Total > 0 {
		fmt.Fprintf(w, "\nregistry volume: %s of %s used, %s free\n",
			bytesText(rep.Used), bytesText(rep.Total), bytesText(rep.Total-rep.Used))
	}
	return nil
}

// progressText is a running mirror's numbers, or "-" when it is not running.
func progressText(e cache.Entry) string {
	if e.Progress == nil {
		return "-"
	}
	return e.Progress.Text()
}

// joinItems is items joined by ", ", or "-" when there are none.
func joinItems(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}
