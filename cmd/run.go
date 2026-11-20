package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/sebrandon1/vsno/lib/ibu"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/release"
	"github.com/sebrandon1/vsno/lib/run"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/setup"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"
)

var (
	rnFile string
	rnOpts run.Options
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Do a whole Image Based Upgrade from only a kubeconfig",
	Long: `run does an Image Based Upgrade end to end:
  1. setup: OpenShift Virtualization, storage and the environment, as vsno setup
  2. vsnos: a seed and a target vSNO, both started before waiting for either
  3. seed: a seed image made from the seed vSNO (stopped afterwards)
  4. upgrade: the target upgraded with it, as vsno upgrade

By default the seed is the newest stable release of the host's OpenShift minor
and the target the one before it; --seed-version and --target-version take
anything vsno create does. --seed-image upgrades to an existing seed image
instead of making one. The plan is shown and confirmed first, unless --yes.

Each stage reuses what's there: a vSNO at the right version, the seed image
made earlier, an upgrade part way. So when a run stops, running it again
carries on. Stages are recorded in the environment's state (vsno status).

-f vsno.yaml takes the same settings as the flags, by their JSON names
(targetVersion, seedName, lca: {ref: ...}); flags override the file.`,
	Args: cobra.NoArgs,
	RunE: runRun,
}

func init() {
	f := runCmd.Flags()
	f.StringVarP(&rnFile, "file", "f", "", "settings file (YAML) with the same settings as the flags")
	f.StringVar(&rnOpts.TargetVersion, "target-version", "", "the target's OpenShift version (default: the release before the seed's)")
	f.StringVar(&rnOpts.SeedVersion, "seed-version", "", "the seed's OpenShift version (default: the newest stable release of the host's minor)")
	f.StringVar(&rnOpts.TargetName, "target-name", "", "the target vSNO's name (default target)")
	f.StringVar(&rnOpts.SeedName, "seed-name", "", "the seed vSNO's name (default seed)")
	f.StringVar(&rnOpts.SeedImage, "seed-image", "", "upgrade to this existing seed image instead of making one")
	f.StringVar(&rnOpts.SeedRepo, "seed-repo", "", "push the seed image to <repo>:<version> instead of the environment's registry")
	f.StringVar(&rnOpts.Node, "node", "", "worker for the vSNOs (default: setup's)")
	f.IntVar(&rnOpts.Cores, "cores", 0, "vCPUs per vSNO (default as vsno create)")
	f.Int64Var(&rnOpts.MemoryGiB, "memory", 0, "GiB of memory per vSNO (default as vsno create)")
	f.BoolVar(&rnOpts.Finalize, "finalize", false, "finalize the upgrade afterwards")
	f.BoolVar(&rnOpts.Rollback, "rollback", false, "roll the upgrade back afterwards")
	f.BoolVar(&rnOpts.Delete, "delete", false, "delete both vSNOs afterwards")
	f.BoolVar(&rnOpts.Yes, "yes", false, "run the plan without asking (for CI)")
	addRunLCAFlags(f)
	rootCmd.AddCommand(runCmd)
}

// addRunLCAFlags binds the --lca-* flags to the run's LCA source.
func addRunLCAFlags(f *pflag.FlagSet) {
	f.StringVar(&rnOpts.LCA.Ref, "lca-ref", "", "build LCA from this ref and use it on both vSNOs (see vsno prepare)")
	f.StringVar(&rnOpts.LCA.Repo, "lca-repo", "", "the git repository --lca-ref is in")
	f.StringVar(&rnOpts.LCA.Image, "lca-image", "", "use this prebuilt LCA image on both vSNOs")
}

// runOptions are the settings file's, with the flags given on top.
func runOptions(cmd *cobra.Command) (run.Options, error) {
	var o run.Options
	if rnFile != "" {
		b, err := os.ReadFile(rnFile) // #nosec G304 -- the user names the file with -f
		if err != nil {
			return o, err
		}
		if err := yaml.UnmarshalStrict(b, &o); err != nil {
			return o, fmt.Errorf("parsing %s: %w", rnFile, err)
		}
	}
	f := rnOpts
	over := map[string]func(){
		"target-version": func() { o.TargetVersion = f.TargetVersion },
		"seed-version":   func() { o.SeedVersion = f.SeedVersion },
		"target-name":    func() { o.TargetName = f.TargetName },
		"seed-name":      func() { o.SeedName = f.SeedName },
		"seed-image":     func() { o.SeedImage = f.SeedImage },
		"seed-repo":      func() { o.SeedRepo = f.SeedRepo },
		"node":           func() { o.Node = f.Node },
		"cores":          func() { o.Cores = f.Cores },
		"memory":         func() { o.MemoryGiB = f.MemoryGiB },
		"finalize":       func() { o.Finalize = f.Finalize },
		"rollback":       func() { o.Rollback = f.Rollback },
		"delete":         func() { o.Delete = f.Delete },
		"yes":            func() { o.Yes = f.Yes },
		"lca-ref":        func() { o.LCA.Ref = f.LCA.Ref },
		"lca-repo":       func() { o.LCA.Repo = f.LCA.Repo },
		"lca-image":      func() { o.LCA.Image = f.LCA.Image },
	}
	flags := cmd.Flags()
	flags.Visit(func(fl *pflag.Flag) {
		if fn, ok := over[fl.Name]; ok {
			fn()
		}
	})
	if o.Namespace == "" || flags.Changed("namespace") {
		o.Namespace = namespace
	}
	return o, nil
}

func runRun(cmd *cobra.Command, _ []string) error {
	o, err := runOptions(cmd)
	if err != nil {
		return err
	}
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	ge, err := guestEnvFor(c, o.Namespace, 0)
	if err != nil {
		return err
	}
	se, err := setupEnv(cmd, c, true, 20*time.Minute)
	if err != nil {
		return err
	}
	resolver := release.Resolver{}
	env := run.Env{
		Env: seed.Env{Env: ge},
		Setup: func(ctx context.Context) error {
			_, err := setup.Run(ctx, se, setup.Options{Namespace: o.Namespace, Settings: precheck.Suggested{Node: o.Node},
				Yes: o.Yes, Images: manifests.DefaultImages()})
			return err
		},
		Stable:  resolver.Stable,
		Confirm: confirmOnTerminal(os.Stdin, cmd.ErrOrStderr()),
	}
	res, err := run.Run(cmd.Context(), env, o)
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if res != nil {
			if jerr := writeJSON(w, res); jerr != nil && err == nil {
				err = jerr
			}
		}
		return err
	}
	if res != nil {
		renderRun(w, res, pal)
	}
	return err
}

// renderRun prints the stages' timings and the upgrade's report.
func renderRun(w io.Writer, res *run.Result, pal ui.Palette) {
	fmt.Fprintf(w, "\n%s\n", pal.Bold("Run"))
	var t ui.Table
	for _, s := range res.Plan.Stages {
		took, ok := res.Timings[s]
		icon := pal.Green("✓")
		switch {
		case !ok:
			icon, took = pal.Dim("="), "not run"
		case slices.Contains(res.Plan.Done, s):
			took += pal.Dim(" (resumed)")
		}
		t.Row("  "+icon, string(s), took)
	}
	t.Write(w)
	if res.Upgrade != nil {
		renderUpgrade(w, res.Upgrade, ibu.UpgradeOptions{Seed: res.Plan.SeedImage}, pal)
	}
}
