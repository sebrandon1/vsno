package cmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
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
settle, installs the Lifecycle Agent from the vSNO's own catalog if it's
missing, makes the vSNO trust the environment's registry, then runs LCA's seed
generation and follows it. The vSNO's cluster is stopped while LCA builds the
image (progress then comes from the node's journal) and comes back afterwards.

The image goes to the environment's registry as seeds/<vsno>:<version>, unless
--image names another (the host cluster's pull secret must have credentials
for its registry). It takes about 15-30 minutes.`,
	Args: cobra.ExactArgs(1),
	RunE: runSeedCreate,
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
the two things vsno can: it subscribes to LCA from the vSNO's own catalog and
makes the vSNO trust the registry, then reports again. seed create runs the
same checks first.`,
	Args: cobra.ExactArgs(1),
	RunE: runSeedPrecheck,
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
	Args: cobra.MinimumNArgs(1),
	RunE: runSeedDelete,
}

func init() {
	f := seedCreateCmd.Flags()
	f.StringVar(&sdImage, "image", "", "push the seed here instead of the environment's registry")
	f.BoolVar(&sdStop, "stop", false, "stop the vSNO afterwards, to free its memory")
	f.DurationVar(&sdTimeout, "timeout", time.Hour, "how long to wait for the seed generation")
	f = seedPrecheckCmd.Flags()
	f.StringVar(&spImage, "image", "", "the seed image reference to check, instead of the environment's registry")
	f.BoolVar(&spInstall, "install", false, "install LCA and set up registry trust where they're missing")
	f.DurationVar(&spTimeout, "timeout", time.Hour, "how long --install may wait for each install")
	seedDeleteCmd.Flags().BoolVar(&sdYes, "yes", false, "don't ask for confirmation")
	seedDeleteCmd.Flags().BoolVar(&sdDryRun, "dry-run", false, "list what would be deleted and change nothing")
	seedCmd.AddCommand(seedCreateCmd, seedPrecheckCmd, seedListCmd, seedDeleteCmd)
	rootCmd.AddCommand(seedCmd)
}

func seedEnv(cmd *cobra.Command, timeout time.Duration) (seed.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return seed.Env{}, err
	}
	env, err := guestEnv(cmd.Context(), c, timeout)
	return seed.Env{Env: env}, err
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
		for _, s := range res.Installed {
			fmt.Fprintf(w, "%s installed %s\n", pal.Icon("✓"), s)
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
		t.Row(img.Ref, img.From, v, bytesText(img.SizeBytes), age(now.Sub(img.Created)))
	}
	t.Write(w)
	return nil
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
