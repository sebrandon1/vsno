package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	dlYes     bool
	dlDryRun  bool
	dlTimeout time.Duration
	pwWait    bool
	pwTimeout time.Duration
)

var deleteCmd = &cobra.Command{
	Use:   "delete <name>...",
	Short: "Delete vSNOs",
	Long: `delete removes each named vSNO: its VM and root disk, the ISO, its Service,
its DNS entry and the builder's files for it. It lists what it will delete and
asks you to type the name(s), unless --yes. --dry-run only lists.`,
	Args:              someArgs("one or more vSNOs, as `vsno list` shows them"),
	ValidArgsFunction: completeVSNOs,
	RunE:              trimmed(runDelete),
}

var startCmd = &cobra.Command{
	Use:               "start <name>",
	Short:             "Start a stopped vSNO",
	Args:              oneArg("the vSNO, as `vsno list` shows it"),
	ValidArgsFunction: completeVSNOFirst,
	RunE:              func(cmd *cobra.Command, args []string) error { return runPower(cmd, args[0], true) },
}

var stopCmd = &cobra.Command{
	Use:               "stop <name>",
	Short:             "Stop a vSNO, keeping its disk (frees its memory)",
	Args:              oneArg("the vSNO, as `vsno list` shows it"),
	ValidArgsFunction: completeVSNOFirst,
	RunE:              func(cmd *cobra.Command, args []string) error { return runPower(cmd, args[0], false) },
}

func init() {
	f := deleteCmd.Flags()
	f.BoolVar(&dlYes, "yes", false, "don't ask for confirmation")
	f.BoolVar(&dlDryRun, "dry-run", false, "list what would be deleted and change nothing")
	f.DurationVar(&dlTimeout, "timeout", 10*time.Minute, "how long to wait for each vSNO to be gone")
	for _, c := range []*cobra.Command{startCmd, stopCmd} {
		c.Flags().BoolVar(&pwWait, "wait", false, "wait until the vSNO is ready (start) or stopped (stop)")
		c.Flags().DurationVar(&pwTimeout, "timeout", 30*time.Minute, "how long to wait for the vSNO to change state")
	}
	rootCmd.AddCommand(deleteCmd, startCmd, stopCmd)
}

func runDelete(cmd *cobra.Command, args []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	env, err := guestEnv(cmd.Context(), c, dlTimeout)
	if err != nil {
		return err
	}
	planShown := false
	res, err := guest.Delete(cmd.Context(), env, guest.DeleteOptions{
		Namespace: namespace, Names: args, DryRun: dlDryRun, Yes: dlYes,
		Confirm: confirmOnTerminal(os.Stdin, cmd.ErrOrStderr()),
		ShowPlan: func(r *guest.DeleteResult) {
			renderDeletePlan(cmd.ErrOrStderr(), r, palette(cmd.ErrOrStderr()))
			fmt.Fprintln(cmd.ErrOrStderr())
			planShown = true
		},
	})
	if rerr := renderDelete(cmd.OutOrStdout(), res, planShown || err != nil, palette(cmd.OutOrStdout())); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

func renderDelete(w io.Writer, res *guest.DeleteResult, planShown bool, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return cliout.PrintJSON(w, res)
	}
	deleted := apply.Count(res.Objects, apply.Removed)
	switch {
	case len(res.Objects) == 0:
	case deleted == len(res.Objects)-apply.Count(res.Objects, apply.Absent):
		fmt.Fprintf(w, "%s %d part(s) deleted.\n", pal.Icon("✓"), deleted)
	case deleted > 0:
		fmt.Fprintf(w, "%s %d of %d part(s) deleted.\n", pal.Icon("!"), deleted, len(res.Objects)-apply.Count(res.Objects, apply.Absent))
	case !planShown:
		renderDeletePlan(w, res, pal)
	}
	return nil
}

func renderDeletePlan(w io.Writer, res *guest.DeleteResult, pal ui.Palette) {
	renderObjects(w, res.Objects, pal)
	note := "Nothing was changed."
	if dlDryRun {
		note = "Dry run: nothing was changed."
	}
	fmt.Fprintf(w, "\n%d part(s): %s. %s\n", len(res.Objects), apply.Tally(res.Objects, "nothing to delete",
		apply.Label{Action: apply.Delete, Text: "to delete"}, apply.Label{Action: apply.Absent, Text: "absent"}), note)
}

func runPower(cmd *cobra.Command, name string, run bool) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	env, err := guestEnv(cmd.Context(), c, pwTimeout)
	if err != nil {
		return err
	}
	se := seed.Env{Env: env}
	se.Defaults(namespace)
	// LCA is generating on the node, with the guest's services stopped: a stop
	// here kills the generation half way and leaves crio and kubelet disabled.
	if !run {
		if g := seed.RunningSeedGen(cmd.Context(), se, namespace, name); g != nil {
			return stopRefusal(name, g)
		}
	}
	if err := guest.SetRunning(cmd.Context(), env, namespace, name, run); err != nil {
		return err
	}
	verb, want := powerWait(run)
	res := powerResult{VSNO: name, Action: strings.TrimSuffix(verb, "ing")}
	if !pwWait {
		if outputFormat == formatJSON {
			return writeJSON(cmd.OutOrStdout(), res)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s; follow it with `vsno list --watch`\n", verb, name)
		return nil
	}
	st, err := guest.WaitState(cmd.Context(), env, namespace, name, want...)
	if run {
		err = startOutcome(cmd, se, name, st, err)
	}
	res.State = string(st)
	if outputFormat == formatJSON {
		if jerr := writeJSON(cmd.OutOrStdout(), res); jerr != nil && err == nil {
			err = jerr
		}
	}
	return err
}

// startOutcome turns a finished start into what it means: a failure, a guest
// that answers nothing because of a seed generation, or nothing to add.
func startOutcome(cmd *cobra.Command, se seed.Env, name string, st guest.State, err error) error {
	switch {
	case err == nil && st == guest.Failed:
		return fmt.Errorf("vSNO %s failed to start; see `vsno list` and `vsno screenshot %s`", name, name)
	case err == nil && st == guest.MakingSeed:
		// It answers nothing with the mark set: LCA is generating.
		if broken, _ := seed.SeedGenInterrupted(cmd.Context(), se.Access(name)); broken {
			return startRecovery(name)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s is making a seed image; `vsno list` follows it\n", name)
		return nil
	case err != nil:
		// The wait timed out: a stop during a generation leaves the services
		// off, and the guest then never answers.
		if broken, _ := seed.SeedGenInterrupted(cmd.Context(), se.Access(name)); broken {
			return startRecovery(name)
		}
	}
	return err
}

// stopRefusal is why a vSNO making a seed image can't be stopped.
func stopRefusal(name string, g *seed.SeedGen) error {
	return fmt.Errorf("%s is making a seed image (%s): stopping it kills the generation half way, and LCA then leaves "+
		"crio and kubelet disabled, so the vSNO will not answer when it starts again. Let it finish — `vsno list` follows "+
		"it — then stop it", name, g)
}

// startRecovery is what a vSNO whose services an interrupted seed generation
// left off needs: `vsno doctor --fix` runs it.
func startRecovery(name string) error {
	return fmt.Errorf("vSNO %s started, but crio and kubelet are disabled: a seed generation was interrupted, and its "+
		"state is still on the node, so the cluster does not come up. Recover with `vsno doctor %s --fix` (or `vsno ssh %s "+
		"-- 'sudo systemctl enable --now crio kubelet'`)", name, name, name)
}

// powerResult is what start and stop report in JSON.
type powerResult struct {
	VSNO   string `json:"vsno"`
	Action string `json:"action"`          // start or stop
	State  string `json:"state,omitempty"` // the state waited for, with --wait
}

// powerWait is what start and stop say they are doing, and the states they wait
// for: a stop wants the VM halted, a start wants the guest answering again — or
// one of the two states that end with it not answering, which startOutcome
// turns into what is going on.
func powerWait(run bool) (verb string, want []guest.State) {
	if run {
		return "starting", []guest.State{guest.Ready, guest.Failed, guest.MakingSeed}
	}
	return "stopping", []guest.State{guest.Stopped}
}
