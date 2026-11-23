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
	Args: someArgs("one or more vSNOs, as `vsno list` shows them"),
	RunE: runDelete,
}

var startCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a stopped vSNO",
	Args:  oneArg("the vSNO, as `vsno list` shows it"),
	RunE:  func(cmd *cobra.Command, args []string) error { return runPower(cmd, args[0], true) },
}

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a vSNO, keeping its disk (frees its memory)",
	Args:  oneArg("the vSNO, as `vsno list` shows it"),
	RunE:  func(cmd *cobra.Command, args []string) error { return runPower(cmd, args[0], false) },
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
	if err == nil && st == guest.Failed {
		err = fmt.Errorf("vSNO %s failed to start; see `vsno list` and `vsno screenshot %s`", name, name)
	}
	res.State = string(st)
	if outputFormat == formatJSON {
		if jerr := writeJSON(cmd.OutOrStdout(), res); jerr != nil && err == nil {
			err = jerr
		}
	}
	return err
}

// powerResult is what start and stop report in JSON.
type powerResult struct {
	VSNO   string `json:"vsno"`
	Action string `json:"action"`          // start or stop
	State  string `json:"state,omitempty"` // the state waited for, with --wait
}

// powerWait is what start and stop say they are doing, and the states they wait
// for: a stop wants the VM halted, a start wants the guest answering again.
func powerWait(run bool) (verb string, want []guest.State) {
	if run {
		return "starting", []guest.State{guest.Ready, guest.Failed}
	}
	return "stopping", []guest.State{guest.Stopped}
}
