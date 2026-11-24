package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/config"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/registry"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	sdImage   string
	sdStop    bool
	sdTimeout time.Duration
	sdYes     bool
	sdDryRun  bool
	spImage   string
	spInstall bool
	spTimeout time.Duration
	ssTo      string
	ssFrom    string
	ssAuth    string
	ssCA      string
	ssTLS     bool
	ssForce   bool
)

var seedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Make and manage Image Based Upgrade seed images",
	Long: `seed makes Image Based Upgrade seed images from vSNOs and manages them in the
environment's registry, as seeds/<vSNO>:<version>.`,
}

var seedCreateCmd = &cobra.Command{
	Use:   "create <vsno>",
	Short: "Make a seed image from a vSNO",
	Long: `create turns a ready vSNO into a seed image. It waits for the cluster to
settle, readies the vSNO the way vsno seed precheck --install does (the
Lifecycle Agent and OADP from its own catalog, and trust in the environment's
registry), then runs LCA's seed generation and follows it. The vSNO's cluster
is stopped while LCA builds the image (progress then comes from the node's
journal) and comes back afterwards.

The image goes to the environment's registry as seeds/<vsno>:<version>, unless
--image names another (the host cluster's pull secret must have credentials
for its registry). It takes about 15-30 minutes.`,
	Args: oneArg("the vSNO to make the seed image from, as `vsno list` shows it"),
	RunE: trimmed(runSeedCreate),
}

var seedPrecheckCmd = &cobra.Command{
	Use:   "precheck <vsno>",
	Short: "Check what a seed generation needs, and set it up with --install",
	Long: `precheck reports everything making a seed image from a vSNO needs, before
starting the 15-30 minute generation: the vSNO is ready, the environment's
registry is usable, the vSNO's API answers and its cluster is settled, the
Lifecycle Agent is installed (or the vSNO's catalog has it), the vSNO trusts
the registry's CA, and the seed image can be pushed.

It only reads, and exits non-zero if anything is missing. --install sets up
what vsno can: the Lifecycle Agent and OADP from the vSNO's own catalog, and
trust in the registry — the same steps vsno prepare takes — then reports
again. seed create runs the same checks first.`,
	Args: oneArg("the vSNO to check, as `vsno list` shows it"),
	RunE: trimmed(runSeedPrecheck),
}

var seedListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the seed images in the environment's registry",
	Args:    cobra.NoArgs,
	RunE:    runSeedList,
}

var seedDeleteCmd = &cobra.Command{
	Use:   "delete <image|vsno:version|vsno>...",
	Short: "Delete seed images from the environment's registry",
	Long: `delete removes seed images from the environment's registry: by full reference,
by <vsno>:<version>, or all of a vSNO's seeds by its name. It lists them and
asks you to type the arguments, unless --yes. --dry-run only lists. The
registry frees their space only when it is garbage collected.`,
	Args: someArgs("the seed images to delete, as `vsno seed list` shows them"),
	RunE: trimmed(runSeedDelete),
}

var seedSyncCmd = &cobra.Command{
	Use:   "sync <vsno>:<version> {--to <repo> | --from <image>}",
	Short: "Copy a seed image to another registry, or bring one in",
	Long: `sync copies a seed image between the environment's registry and another one.

--to pushes a seed out: the repository you name gets the seed's own tag, so
--to quay.io/me/seeds puts seeds/<vsno>:<version> there under <version>.

--from brings an image in, stored as <vsno>:<version>, so the version has to
come with it. A peer registry on a private CA needs --ca as well as --authfile.

Credentials for the registry that is not the environment's come from your
container auth file, the first one that covers it, or --authfile; nothing is
written to the cluster. An existing tag on the far side is left alone unless
--overwrite. The copy runs in the builder pod, so the image never passes
through your machine.`,
	Args: oneArg("the seed as <vsno>[:<version>], as `vsno seed list` shows it"),
	RunE: trimmed(runSeedSync),
}

func init() {
	f := seedCreateCmd.Flags()
	f.StringVar(&sdImage, "image", "", "push the seed here instead of the environment's registry")
	f.BoolVar(&sdStop, "stop", false, "stop the vSNO afterwards, to free its memory")
	f.DurationVar(&sdTimeout, "timeout", time.Hour, "how long to wait for the seed generation")
	f = seedPrecheckCmd.Flags()
	f.StringVar(&spImage, "image", "", "the seed image reference to check, instead of the environment's registry")
	f.BoolVar(&spInstall, "install", false, "install LCA and OADP, and set up registry trust, where they're missing")
	f.DurationVar(&spTimeout, "timeout", time.Hour, "how long --install may wait for each install")
	seedDeleteCmd.Flags().BoolVar(&sdYes, "yes", false, "don't ask for confirmation")
	seedDeleteCmd.Flags().BoolVar(&sdDryRun, "dry-run", false, "list what would be deleted and change nothing")
	f = seedSyncCmd.Flags()
	f.StringVar(&ssTo, "to", "", "copy the seed to this repository")
	f.StringVar(&ssFrom, "from", "", "copy the seed from this image")
	f.StringVar(&ssAuth, "authfile", "", "credentials for the other registry (default: your container auth file)")
	f.StringVar(&ssCA, "ca", "", "the other registry's CA certificate, when it is not publicly trusted")
	f.BoolVar(&ssTLS, "insecure", false, "the other registry may be plain HTTP or hold a certificate this environment does not trust")
	f.BoolVar(&ssForce, "overwrite", false, "replace the other registry's tag when it already exists")
	seedCmd.AddCommand(seedCreateCmd, seedPrecheckCmd, seedListCmd, seedDeleteCmd, seedSyncCmd)
	rootCmd.AddCommand(seedCmd)
}

func seedEnv(cmd *cobra.Command, timeout time.Duration) (seed.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return seed.Env{}, err
	}
	env, err := guestEnv(cmd.Context(), c, timeout)
	return seed.Env{Env: env, Volume: func(ctx context.Context, ns string) (int64, int64, error) {
		return registry.Usage(ctx, c.Config, c.Kube, ns)
	}}, err
}

func runSeedCreate(cmd *cobra.Command, args []string) error {
	env, err := seedEnv(cmd, sdTimeout)
	if err != nil {
		return err
	}
	if outputFormat != formatJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Making a seed image from vSNO %s in %s...\n\n", args[0], namespace)
	}
	res, err := seed.Create(cmd.Context(), env, seed.CreateOptions{Namespace: namespace, Name: args[0], Image: sdImage, Stop: sdStop})
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		return writeJSON(w, res)
	}
	fmt.Fprintf(w, "\n%s Seed image %s (OpenShift %s, LCA %s) in %s.\n", pal.Icon("✓"), pal.Bold(res.Image), res.Version, res.LCA, ui.FormatElapsed(res.Duration))
	if res.Stopped {
		fmt.Fprintf(w, "vSNO %s is stopped; `vsno start %s` brings it back.\n", res.Name, res.Name)
	}
	fmt.Fprintln(w, "See it with `vsno seed list`.")
	return nil
}

// seedSections are the seed precheck's report sections, in order.
var seedSections = []reportSection{
	{seed.GroupVSNO, "vSNO"},
	{precheck.GroupCluster, "Cluster"},
	{seed.GroupRegistry, "Registry"},
	{seed.GroupLCA, "Lifecycle Agent"},
	{seed.GroupTarget, "Seed target"},
}

func runSeedPrecheck(cmd *cobra.Command, args []string) error {
	env, err := seedEnv(cmd, spTimeout)
	if err != nil {
		return err
	}
	if outputFormat != formatJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Checking what a seed from vSNO %s in %s needs...\n\n", args[0], namespace)
	}
	res, err := seed.Precheck(cmd.Context(), env, seed.PrecheckOptions{
		Namespace: namespace, Name: args[0], Image: spImage, Install: spInstall,
	})
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if err := writeJSON(w, res); err != nil {
			return err
		}
	} else {
		renderSections(w, res.Results, nil, "", seedSections, pal)
		for _, s := range res.Ready {
			fmt.Fprintf(w, "%s %s\n", pal.Icon("✓"), s)
		}
		for _, s := range res.Warnings {
			fmt.Fprintf(w, "%s %s\n", pal.Yellow("!"), s)
		}
		if res.Failed() > 0 {
			if !spInstall {
				fmt.Fprintf(w, "\nRun `vsno seed precheck %s --install` to set up what can be set up.\n", args[0])
			}
		} else {
			fmt.Fprintf(w, "\n%s Ready: `vsno seed create %s` makes the seed image %s.\n", pal.Icon("✓"), args[0], res.Image)
		}
	}
	if n := res.Failed(); n > 0 {
		return fmt.Errorf("seed precheck: %d check(s) failed", n)
	}
	return nil
}

func runSeedList(cmd *cobra.Command, _ []string) error {
	env, err := seedEnv(cmd, 0)
	if err != nil {
		return err
	}
	imgs, err := seed.List(cmd.Context(), env, namespace)
	if err != nil {
		return err
	}
	return renderSeeds(cmd.OutOrStdout(), imgs, time.Now(), palette(cmd.OutOrStdout()))
}

func renderSeeds(w io.Writer, imgs []seed.Image, now time.Time, pal ui.Palette) error {
	if outputFormat == formatJSON {
		if imgs == nil {
			imgs = []seed.Image{}
		}
		return writeJSON(w, imgs)
	}
	if len(imgs) == 0 {
		fmt.Fprintf(w, "No seed images in %s. Make one with `vsno seed create <vsno>`.\n", namespace)
		return nil
	}
	var t ui.Table
	t.Row(pal.Bold("IMAGE"), pal.Bold("FROM"), pal.Bold("VERSION"), pal.Bold("SIZE"), pal.Bold("AGE"))
	for _, img := range imgs {
		v := img.Version
		if v == "" {
			v = "-"
		}
		t.Row(img.Ref, img.From, v, bytesText(img.SizeBytes), seedAge(now, img))
	}
	t.Write(w)
	return nil
}

// seedAge is how long the seed has been here: when it was copied in if it was,
// and otherwise the image's own build time. The table says which, because an
// image pulled in a minute ago would otherwise read as old as its upstream.
func seedAge(now time.Time, img seed.Image) string {
	if img.SyncedAt != nil {
		return ageSince(now, *img.SyncedAt) + " (synced)"
	}
	return ageSince(now, img.Created)
}

// bytesText renders a size in GiB or MiB.
func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	case n > 0:
		return fmt.Sprintf("%dMi", (n+(1<<20)-1)>>20)
	}
	return "-"
}

func runSeedSync(cmd *cobra.Command, args []string) error {
	env, err := seedEnv(cmd, 0)
	if err != nil {
		return err
	}
	to, auth, ca := syncDefaults()
	if outputFormat != formatJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Copying seed image %s...\n\n", args[0])
	}
	res, err := seed.Sync(cmd.Context(), env, seed.SyncOptions{
		Namespace: namespace, Seed: args[0], To: to, From: ssFrom,
		AuthFile: auth, CAPath: ca, Insecure: ssTLS, Overwrite: ssForce,
	})
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		return writeJSON(w, res)
	}
	renderSeedSync(w, res, pal)
	return nil
}

// syncDefaults is where the copy goes and how to reach it: the flag, then the
// environment, then the settings file. A configured destination applies only
// when neither --to nor --from was given, so a pull never inherits a --to and
// fail its direction check.
func syncDefaults() (string, string, string) {
	file := configFor()
	toSet, _ := config.SettingFor(config.SeedSyncTo)
	authSet, _ := config.SettingFor(config.SeedSyncAuthFile)
	caSet, _ := config.SettingFor(config.SeedSyncCA)
	to := ssTo
	if ssTo == "" && ssFrom == "" {
		to = config.Resolve(toSet, "", file, false)
	}
	return to, config.Resolve(authSet, ssAuth, file, true), config.Resolve(caSet, ssCA, file, true)
}

// renderSeedSync prints where one seed image went and what the far side holds.
func renderSeedSync(w io.Writer, res *seed.SyncResult, pal ui.Palette) {
	what := res.Name
	if res.Version != "" {
		what += ":" + res.Version
	}
	fmt.Fprintf(w, "%s synced %s %s\n", pal.Icon("✓"), pal.Bold(what), pal.Dim(bytesText(res.SizeBytes)))
	// The label is padded before coloring, so the color codes cannot shift it.
	fmt.Fprintf(w, "  %s %s\n", pal.Dim(fmt.Sprintf("%-6s", "from")), res.Source)
	fmt.Fprintf(w, "  %s %s\n", pal.Dim(fmt.Sprintf("%-6s", "to")), res.Destination)
	if res.Digest != "" {
		fmt.Fprintf(w, "  %s %s\n", pal.Dim(fmt.Sprintf("%-6s", "digest")), res.Digest)
	}
}

func runSeedDelete(cmd *cobra.Command, args []string) error {
	env, err := seedEnv(cmd, 0)
	if err != nil {
		return err
	}
	ew := cmd.ErrOrStderr()
	planShown := false
	res, err := seed.Delete(cmd.Context(), env, seed.DeleteOptions{
		Namespace: namespace, Args: args, DryRun: sdDryRun, Yes: sdYes,
		Confirm: confirmOnTerminal(os.Stdin, ew),
		ShowPlan: func(imgs []seed.Image) {
			renderSeedPlan(ew, imgs, palette(ew))
			fmt.Fprintln(ew)
			planShown = true
		},
	})
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if rerr := writeJSON(w, res); rerr != nil && err == nil {
			err = rerr
		}
		return err
	}
	switch {
	case len(res.Deleted) > 0:
		fmt.Fprintf(w, "%s %d seed image(s) deleted. The registry frees their space when it is garbage collected.\n", pal.Icon("✓"), len(res.Deleted))
	case !planShown && len(res.Images) > 0:
		renderSeedPlan(w, res.Images, pal)
		if sdDryRun {
			fmt.Fprintln(w, "\nDry run: nothing was changed.")
		}
	}
	return err
}

func renderSeedPlan(w io.Writer, imgs []seed.Image, pal ui.Palette) {
	for _, img := range imgs {
		fmt.Fprintf(w, "  %s %s  %s\n", pal.Red("delete"), img.Ref, pal.Dim(bytesText(img.SizeBytes)))
	}
}

func writeJSON(w io.Writer, v any) error {
	return cliout.PrintJSON(w, v)
}
