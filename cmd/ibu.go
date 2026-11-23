package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/ibu"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	lcaSource     ibu.LCASource
	prRole        string
	upSeed        string
	upSeedVersion string
	upTo          string
	upFinalize    bool
	upRollback    bool
	upAbort       bool
)

var prepareCmd = &cobra.Command{
	Use:   "prepare <vsno>...",
	Short: "Get vSNOs ready for an Image Based Upgrade",
	Long: `prepare installs what an Image Based Upgrade needs on each vSNO, reusing
what's already there: the Lifecycle Agent and OADP from the vSNO's own catalog,
and trust in the environment's registry. These are the same steps vsno seed
precheck --install takes, so both leave a vSNO in the same state; LCA's own
prerequisites have OADP on the seed as well as the target, so both roles get
it.

A target (the default role) also gets a backup location in the environment's S3
for OADP, waiting until it is Available, and the pull secret for the seed
image. vsno upgrade prepares its target itself, so this is for checking a vSNO
ahead of time.`,
	Args: someArgs("one or more vSNOs to prepare, as `vsno list` shows them"),
	RunE: runPrepare,
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade <target> {--seed <vsno|image> | --abort}",
	Short: "Upgrade a vSNO to a seed image's version with an Image Based Upgrade",
	Long: `upgrade runs an Image Based Upgrade on the target vSNO with the Lifecycle
Agent. --seed is a seed image (vsno seed list) or a vSNO, meaning its newest
seed image; for an image outside the environment's registry, also pass
--seed-version.

It prepares the target (see vsno prepare), records its identity, then runs
Prep (the seed's images are pulled ahead, shown as a progress bar) and Upgrade
(the node reboots into the new version; its API is down for about 15 minutes).
It reports the identity before and after, which must differ only in version,
and the time each stage and phase took.

--to prep stops after Prep. --finalize goes on to Idle, dropping the old
version; --rollback goes back to it. --abort cancels a Prep (one that failed,
say), moving the target back to Idle. Run again with the same seed, upgrade
carries on from where the target is, so a finished upgrade can be finalized
later with --finalize.`,
	Args: oneArg("the vSNO to upgrade, as `vsno list` shows it"),
	RunE: runUpgrade,
}

// addLCAFlags lets a command take LCA from a git ref or an image instead of
// the vSNO's catalog.
func addLCAFlags(f *pflag.FlagSet) {
	f.StringVar(&lcaSource.Ref, "lca-ref", "", "build LCA from this branch, tag or SHA in the cluster and deploy it, instead of the catalog's")
	f.StringVar(&lcaSource.Repo, "lca-repo", "", "the git repository --lca-ref is in (default "+ibu.DefaultLCARepo+"); forks work")
	f.StringVar(&lcaSource.Image, "lca-image", "", "deploy this prebuilt LCA image instead of building one, with --lca-ref's manifests (default main)")
}

func init() {
	prepareCmd.Flags().StringVar(&prRole, "role", string(ibu.RoleTarget), "what the vSNOs are for: seed (LCA, OADP and trust) or target (those, plus a backup location and the seed pull secret)")
	addLCAFlags(prepareCmd.Flags())
	addLCAFlags(upgradeCmd.Flags())
	f := upgradeCmd.Flags()
	f.StringVar(&upSeed, "seed", "", "the seed: an image, <vsno>:<version>, or a vSNO for its newest seed (required unless --abort)")
	f.StringVar(&upSeedVersion, "seed-version", "", "the seed's OpenShift version, for an image outside the environment's registry")
	f.StringVar(&upTo, "to", "upgrade", "stop after this stage: prep or upgrade")
	f.BoolVar(&upFinalize, "finalize", false, "finalize the upgrade (stage Idle), dropping the old version")
	f.BoolVar(&upRollback, "rollback", false, "roll the upgrade back to the old version")
	f.BoolVar(&upAbort, "abort", false, "cancel the target's Prep, moving it back to Idle (no --seed)")
	rootCmd.AddCommand(prepareCmd, upgradeCmd)
}

func runPrepare(cmd *cobra.Command, args []string) error {
	role := ibu.Role(prRole)
	if role != ibu.RoleSeed && role != ibu.RoleTarget {
		return fmt.Errorf("--role takes seed or target, not %q", prRole)
	}
	env, err := seedEnv(cmd, 0)
	if err != nil {
		return err
	}
	if outputFormat != formatJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Preparing %s in %s as %s...\n\n", strings.Join(args, ", "), namespace, role)
	}
	out, err := ibu.Prepare(cmd.Context(), env, ibu.PrepareOptions{Namespace: namespace, Names: args, Role: role, LCA: lcaSource})
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		return writeJSON(w, out)
	}
	fmt.Fprintln(w)
	var t ui.Table
	t.Row(pal.Bold("NAME"), pal.Bold("ROLE"), pal.Bold("LCA"), pal.Bold("OADP"))
	for _, p := range out {
		t.Row(p.Name, string(p.Role), p.LCA, p.OADP)
	}
	t.Write(w)
	for _, p := range out {
		for _, warn := range p.Warnings {
			fmt.Fprintf(w, "%s %s: %s\n", pal.Yellow("!"), p.Name, warn)
		}
	}
	if len(out) > 0 && out[0].Built != nil {
		fmt.Fprintf(w, "\nLCA built from %s at %s (version %s): %s\n", out[0].Built.Repo, out[0].Built.Ref, orDash(out[0].Built.Version), out[0].Built.Image)
	}
	return nil
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	to, ok := map[string]ibu.Stage{"prep": ibu.StagePrep, "upgrade": ibu.StageUpgrade}[strings.ToLower(upTo)]
	if !ok {
		return fmt.Errorf("--to takes prep or upgrade, not %q", upTo)
	}
	env, err := seedEnv(cmd, 0)
	if err != nil {
		return err
	}
	o := ibu.UpgradeOptions{Namespace: namespace, Target: args[0], Seed: upSeed, SeedVersion: upSeedVersion,
		To: to, Finalize: upFinalize, Rollback: upRollback, Abort: upAbort, LCA: lcaSource}
	if outputFormat != formatJSON {
		if upAbort {
			fmt.Fprintf(cmd.ErrOrStderr(), "Canceling the Prep of vSNO %s in %s...\n\n", args[0], namespace)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "Upgrading vSNO %s in %s with seed %s...\n\n", args[0], namespace, upSeed)
		}
	}
	res, err := ibu.Upgrade(cmd.Context(), env, o)
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
		renderUpgrade(w, res, o, pal)
	}
	return err
}

// renderUpgrade prints what an upgrade did: the seed, the identity before and
// after, the stage history and what to run next.
func renderUpgrade(w io.Writer, res *ibu.UpgradeResult, o ibu.UpgradeOptions, pal ui.Palette) {
	fmt.Fprintln(w)
	if res.SeedImage != "" {
		fmt.Fprintf(w, "Seed %s (OpenShift %s)\n", pal.Bold(res.SeedImage), res.SeedVersion)
	}
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "%s %s\n", pal.Yellow("!"), warn)
	}
	if res.After.Version != "" {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("Identity"))
		var t ui.Table
		t.Row("", pal.Dim("before"), pal.Dim("after"))
		for _, r := range [][3]string{
			{"version", res.Before.Version, res.After.Version},
			{"clusterID", res.Before.ClusterID, res.After.ClusterID},
			{"infrastructureName", res.Before.InfrastructureName, res.After.InfrastructureName},
			{"node", res.Before.Node, res.After.Node},
			{"API", res.Before.APIURL, res.After.APIURL},
		} {
			t.Row("  "+r[0], orDash(r[1]), orDash(r[2]))
		}
		t.Write(w)
		for _, c := range res.Changed {
			fmt.Fprintf(w, "%s %s\n", pal.Red("✗"), pal.Red("changed: "+c+" (an IBU must keep the cluster's identity)"))
		}
	}
	if len(res.History) > 0 {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("Stages"))
		renderHistory(w, res.History, pal)
	}
	fmt.Fprintf(w, "\nThe target is in stage %s.", pal.Bold(orDash(string(res.Stage))))
	if hint := nextStep(res, o); hint != "" {
		fmt.Fprintf(w, " %s", hint)
	}
	fmt.Fprintln(w)
}

// renderHistory lists each stage and its phases with how long they took.
func renderHistory(w io.Writer, hist []ibu.History, pal ui.Palette) {
	var t ui.Table
	for _, h := range hist {
		t.Row("  "+string(h.Stage), took(h.StartTime, h.CompletionTime))
		for _, p := range h.Phases {
			t.Row("    "+pal.Dim(p.Phase), pal.Dim(took(p.StartTime, p.CompletionTime)))
		}
	}
	t.Write(w)
}

// took is how long from start to end took, or "running" when it hasn't ended.
func took(start, end time.Time) string {
	if end.IsZero() {
		return "running"
	}
	return ui.FormatElapsed(end.Sub(start))
}

// nextStep says what's left after an upgrade that stopped short of Idle.
func nextStep(res *ibu.UpgradeResult, o ibu.UpgradeOptions) string {
	again := fmt.Sprintf("`vsno upgrade %s --seed %s", res.Target, o.Seed)
	switch res.Stage {
	case ibu.StagePrep:
		return "Continue with " + again + "`."
	case ibu.StageUpgrade:
		return "Keep it with " + again + " --finalize`, or go back with " + again + " --rollback`."
	}
	return ""
}
