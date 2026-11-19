package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

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
)

var deleteCmd = &cobra.Command{
	Use:   "delete <name>...",
	Short: "Delete vSNOs",
	Long: `delete removes each named vSNO: its VM and root disk, the ISO, its Service,
its DNS entry and the builder's files for it. It lists what it will delete and
asks you to type the name(s), unless --yes. --dry-run only lists.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runDelete,
}

var startCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a stopped vSNO",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runPower(cmd, args[0], true) },
}

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a vSNO, keeping its disk (frees its memory)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runPower(cmd, args[0], false) },
}

func init() {
	f := deleteCmd.Flags()
	f.BoolVar(&dlYes, "yes", false, "don't ask for confirmation")
	f.BoolVar(&dlDryRun, "dry-run", false, "list what would be deleted and change nothing")
	f.DurationVar(&dlTimeout, "timeout", 10*time.Minute, "how long to wait for each vSNO to be gone")
	for _, c := range []*cobra.Command{startCmd, stopCmd} {
		c.Flags().BoolVar(&pwWait, "wait", false, "wait until the vSNO is ready (start) or stopped (stop)")
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
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
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
	env, err := guestEnv(cmd.Context(), c, 30*time.Minute)
	if err != nil {
		return err
	}
	if err := guest.SetRunning(cmd.Context(), env, namespace, name, run); err != nil {
		return err
	}
	verb, want := "stopping", []guest.State{guest.Stopped}
	if run {
		verb, want = "starting", []guest.State{guest.Ready, guest.Failed}
	}
	if !pwWait {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s; follow it with `vsno list --watch`\n", verb, name)
		return nil
	}
	st, err := guest.WaitState(cmd.Context(), env, namespace, name, want...)
	if err == nil && st == guest.Failed {
		err = fmt.Errorf("vSNO %s failed to start; see `vsno list` and `vsno screenshot %s`", name, name)
	}
	return err
}
