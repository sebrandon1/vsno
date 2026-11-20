package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/release"
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

<version> takes what create accepts: 4.20.46, 4.20, a nightly, or a release
image pullspec. Nightlies need registry.ci.openshift.org credentials in the
cluster's pull secret, which create already requires for them.`,
	Args: cobra.ExactArgs(1),
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
in a Job, and starts it again; it refuses while a release mirror is running.`,
	Args: cobra.NoArgs,
	RunE: runCacheGC,
}

var cacheListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the mirrored releases, their size, and the vSNOs using them",
	Args:    cobra.NoArgs,
	RunE:    runCacheList,
}

var (
	cgYes    bool
	cpKeep   int
	cpYes    bool
	cpDryRun bool
	cpNoGC   bool
)

func init() {
	f := cachePruneCmd.Flags()
	f.IntVar(&cpKeep, "keep", 0, "keep the N most recently used unused releases")
	f.BoolVar(&cpYes, "yes", false, "don't ask for confirmation")
	f.BoolVar(&cpDryRun, "dry-run", false, "list what would be removed and change nothing")
	f.BoolVar(&cpNoGC, "no-gc", false, "skip the registry garbage collection, which is what frees the space")
	cacheGCCmd.Flags().BoolVar(&cgYes, "yes", false, "don't ask for confirmation")
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
		return err
	}
	start := time.Now()
	ps := newProgress().Start("Mirroring " + args[0])
	p, err := cache.Ensure(cmd.Context(), env, cache.EnsureOptions{Namespace: namespace, Release: rel, OnLine: ps.Update})
	if err != nil {
		ps.Fail(err.Error())
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		ps.Done("")
		return writeJSON(w, p)
	}
	if p.Hit {
		ps.Done("already cached")
		fmt.Fprintf(w, "%s %s is already in %s.\n", pal.Icon("✓"), p.Image, p.Repo)
		return nil
	}
	elapsed := time.Since(start)
	ps.Done("mirrored in " + ui.FormatElapsed(elapsed))
	fmt.Fprintf(w, "%s Mirrored %s into %s: %s in %s (%s).\n", pal.Icon("✓"), p.Image, p.Repo,
		bytesText(p.SizeBytes), ui.FormatElapsed(elapsed), cache.Rate(p.SizeBytes, elapsed))
	fmt.Fprintln(w, "vSNOs of this version now install from the registry.")
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
	env, _, err := cacheEnv(cmd)
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if !cgYes {
		q := fmt.Sprintf("This stops the registry in %s for a few seconds to collect its garbage. Type %q to continue", namespace, "gc")
		if !confirmOnTerminal(os.Stdin, cmd.ErrOrStderr())(q, "gc") {
			return errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	start := time.Now()
	ps := newProgress().Start("Collecting the registry's garbage")
	before, _, _ := env.Volume(cmd.Context(), namespace)
	if err := cache.GarbageCollect(cmd.Context(), env, namespace); err != nil {
		ps.Fail(err.Error())
		return err
	}
	after, _, _ := env.Volume(cmd.Context(), namespace)
	elapsed := ui.FormatElapsed(time.Since(start))
	ps.Done("done in " + elapsed)
	freed := "nothing was left to free"
	if before > after {
		freed = "freeing " + bytesText(before-after)
	}
	fmt.Fprintf(w, "%s Collected the registry's garbage in %s, %s.\n", pal.Icon("✓"), elapsed, freed)
	return nil
}

func renderCachePlan(w io.Writer, p *cache.PrunePlan, pal ui.Palette) {
	for _, e := range p.Entries {
		fmt.Fprintf(w, "  %s %s  %s  %s\n", pal.Red("remove"), dash(e.Version), e.Repo, pal.Dim(bytesText(e.SizeBytes)))
	}
	fmt.Fprintf(w, "\n%s\n", pal.Dim("Frees about "+bytesText(p.Bytes)+" once the registry's garbage is collected."))
}

// releaseUsers maps a release image to the vSNOs running it.
func releaseUsers(cmd *cobra.Command, genv guest.Env) (map[string][]string, error) {
	users := map[string][]string{}
	gs, err := guest.List(cmd.Context(), genv, namespace)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		if g.Image != "" {
			users[g.Image] = append(users[g.Image], g.Name)
		}
	}
	return users, nil
}

func runCacheList(cmd *cobra.Command, _ []string) error {
	env, genv, err := cacheEnv(cmd)
	if err != nil {
		return err
	}
	users, err := releaseUsers(cmd, genv)
	if err != nil {
		return err
	}
	rep, err := cache.List(cmd.Context(), env, namespace, users)
	if err != nil {
		return err
	}
	return renderCache(cmd.OutOrStdout(), rep, time.Now(), palette(cmd.OutOrStdout()))
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
	t.Row(pal.Bold("VERSION"), pal.Bold("IMAGE"), pal.Bold("SIZE"), pal.Bold("MIRRORED"), pal.Bold("LAST USED"), pal.Bold("VSNOS"))
	for _, e := range rep.Entries {
		t.Row(dash(e.Version), e.Image, bytesText(e.SizeBytes), age(now.Sub(e.Mirrored)), age(now.Sub(e.LastUsed)), joinItems(e.Users))
	}
	t.Write(w)
	if rep.Total > 0 {
		fmt.Fprintf(w, "\nregistry volume: %s of %s used, %s free\n",
			bytesText(rep.Used), bytesText(rep.Total), bytesText(rep.Total-rep.Used))
	}
	return nil
}

// joinItems is items joined by ", ", or "-" when there are none.
func joinItems(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}
