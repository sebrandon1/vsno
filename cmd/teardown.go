package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/teardown"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
)

var (
	tdHost    bool
	tdForce   bool
	tdYes     bool
	tdDryRun  bool
	tdTimeout time.Duration
)

var teardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Delete a vSNO environment",
	Long: `teardown deletes the environment namespace: its VMs, support services,
volumes and generated credentials. It only deletes a namespace vsno created.

With --host it also removes what setup installed on the host cluster:
HyperConverged, the LVMCluster (which releases the disk), and the OpenShift
Virtualization and LVM Storage operators. It refuses while VMs or LVMS
volumes outside the namespace exist, or while LVMS has an LVMCluster vsno
didn't create, unless --force.

It lists what it will delete and asks you to type the namespace (or "host"
with --host), unless --yes. --dry-run only lists.`,
	RunE: runTeardown,
}

func init() {
	f := teardownCmd.Flags()
	f.BoolVar(&tdHost, "host", false, "also remove HyperConverged, the LVMCluster and both operators")
	f.BoolVar(&tdForce, "force", false, "ignore the safety checks (VMs or volumes elsewhere, a namespace vsno didn't create)")
	f.BoolVar(&tdYes, "yes", false, "don't ask for confirmation")
	f.BoolVar(&tdDryRun, "dry-run", false, "list what would be deleted and change nothing")
	f.DurationVar(&tdTimeout, "timeout", 10*time.Minute, "how long to wait for each deletion")
	rootCmd.AddCommand(teardownCmd)
}

func runTeardown(cmd *cobra.Command, _ []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	o := teardown.Options{Namespace: namespace, Host: tdHost, Force: tdForce, Yes: tdYes, DryRun: tdDryRun}
	planShown := false
	dc, err := discovery.NewDiscoveryClientForConfig(c.Config)
	if err != nil {
		return err
	}
	env := teardown.Env{Kube: c.Kube, Dyn: c.Dyn, Planner: apply.NewSSA(dc, c.Dyn), Progress: precheckProgress{newProgress()}, Timeout: tdTimeout,
		Confirm: confirmOnTerminal(os.Stdin, cmd.ErrOrStderr()),
		ShowPlan: func(r *teardown.Result) {
			renderTeardownPlan(cmd.ErrOrStderr(), r, palette(cmd.ErrOrStderr()))
			fmt.Fprintln(cmd.ErrOrStderr())
			planShown = true
		},
	}
	if outputFormat != formatJSON {
		fmt.Fprint(cmd.ErrOrStderr(), envNote(cmd.Context(), c.Kube, namespace))
		fmt.Fprintf(cmd.ErrOrStderr(), "Tearing down %s in %s (context %q)...\n\n", namespace, c.Config.Host, c.Context)
	}
	res, err := teardown.Run(cmd.Context(), env, o)
	if rerr := renderTeardown(w, res, err, planShown, palette(w)); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// renderTeardown prints the blockers and, mirroring setup, the steps, then
// either the plan (nothing deleted yet) or what was deleted. planShown means
// the confirmation prompt already showed the plan; it is not repeated unless
// something was deleted since.
func renderTeardown(w io.Writer, res *teardown.Result, runErr error, planShown bool, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return cliout.PrintJSON(w, res)
	}
	for _, b := range res.Blockers {
		if res.Forced {
			fmt.Fprintf(w, "%s  %s %s\n", pal.Icon(statusIcon[precheck.Warn]), b, pal.Dim("(ignored: --force)"))
		} else {
			fmt.Fprintf(w, "%s  %s\n", pal.Icon(statusIcon[precheck.Fail]), b)
		}
	}
	if len(res.Targets) == 0 {
		if runErr == nil {
			fmt.Fprintf(w, "Nothing to delete: namespace %s does not exist.\n", res.Namespace)
		}
		return nil
	}
	deleted := apply.Count(res.Changes(), apply.Removed)
	if planShown && deleted == 0 {
		return nil
	}
	if deleted == 0 {
		renderTeardownPlan(w, res, pal)
		return nil
	}
	renderTeardownSteps(w, res, pal)
	fmt.Fprintf(w, "\n%d object(s) deleted, and everything else in namespace %s.\n", deleted, res.Namespace)
	return nil
}

func renderTeardownSteps(w io.Writer, res *teardown.Result, pal ui.Palette) {
	rows := make([][3]string, 0, len(res.Steps))
	for _, s := range res.Steps {
		rows = append(rows, [3]string{s.Name, string(s.Outcome), s.Detail})
	}
	renderSteps(w, pal, rows)
}

// renderTeardownPlan prints the steps and objects teardown would delete, and
// warns about what setup didn't create but the namespace deletion removes too.
func renderTeardownPlan(w io.Writer, res *teardown.Result, pal ui.Palette) {
	if res.RestoreDensity != nil {
		fmt.Fprintf(w, "%s HyperConverged goes back to %s, as before vsno setup changed it.\n", pal.Icon("i"), res.RestoreDensity)
	}
	renderTeardownSteps(w, res, pal)
	ch := res.Changes()
	renderObjects(w, ch, pal)
	note := "Nothing was changed."
	if tdDryRun {
		note = "Dry run: nothing was changed."
	}
	fmt.Fprintf(w, "\n%d object(s): %s. %s\n", len(ch), teardown.PlanDetail(ch), note)
	if n := len(res.Unmanaged); n > 0 {
		fmt.Fprintf(w, "%s %s\n", pal.Icon("!"), pal.Yellow(fmt.Sprintf(
			"Deleting namespace %s also deletes what setup didn't create (%d): %s.", res.Namespace, n, strings.Join(res.Unmanaged, ", "))))
	}
}
