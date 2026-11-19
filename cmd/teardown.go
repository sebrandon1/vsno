package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/teardown"
	"github.com/spf13/cobra"
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
	env := teardown.Env{Kube: c.Kube, Dyn: c.Dyn, Progress: precheckProgress{newProgress()}, Timeout: tdTimeout,
		Confirm:  confirmOnTerminal(os.Stdin, cmd.ErrOrStderr()),
		ShowPlan: func(r *teardown.Result) { renderTargets(cmd.ErrOrStderr(), r); fmt.Fprintln(cmd.ErrOrStderr()) },
	}
	if outputFormat == "table" {
		fmt.Fprintf(cmd.ErrOrStderr(), "Tearing down %s in %s (context %q)...\n\n", namespace, c.Config.Host, c.Context)
	}
	res, err := teardown.Run(cmd.Context(), env, o)
	if rerr := renderTeardown(w, res, err); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

func renderTeardown(w io.Writer, res *teardown.Result, runErr error) error {
	if outputFormat == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	for _, b := range res.Blockers {
		fmt.Fprintf(w, "✗ %s\n", b)
	}
	if len(res.Targets) == 0 {
		if runErr == nil {
			fmt.Fprintf(w, "Nothing to delete: namespace %s does not exist.\n", res.Namespace)
		}
		return nil
	}
	renderTargets(w, res)
	return nil
}

func renderTargets(w io.Writer, res *teardown.Result) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, t := range res.Targets {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Outcome, t.Ref(), t.Detail)
	}
	_ = tw.Flush()
}
