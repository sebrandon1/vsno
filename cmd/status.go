package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/state"
	"github.com/sebrandon1/vsno/lib/status"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

// statusStages is how many recent stages the table shows; JSON has them all.
const statusStages = 5

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the state of a vSNO environment",
	Long: `status reports, without changing anything:

  - the host components setup installs: the OpenShift Virtualization and
    LVM Storage operators, HyperConverged, the LVMCluster and StorageClass
  - the support services in the namespace and the registry endpoint
  - the VMs in the namespace
  - the most recent stages recorded in ConfigMap <namespace>/vsno-state

It exits non-zero when the namespace is missing or a component is missing or
failing.`,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, _ []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	r, err := status.Collect(cmd.Context(), c.Kube, c.Dyn, namespace)
	if err != nil {
		return err
	}
	if err := renderStatus(cmd.OutOrStdout(), r, palette(cmd.OutOrStdout())); err != nil {
		return err
	}
	if !r.Healthy() {
		return errors.New("status: the environment is not ready")
	}
	return nil
}

func renderStatus(w io.Writer, r *status.Report, pal ui.Palette) error {
	if outputFormat == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	switch {
	case !r.Exists:
		fmt.Fprintln(w, pal.Red(fmt.Sprintf("Namespace %s does not exist; run `vsno setup -n %s`.", r.Namespace, r.Namespace)))
	case !r.Managed:
		fmt.Fprintln(w, pal.Yellow(fmt.Sprintf("Namespace %s was not created by vsno.", r.Namespace)))
	case r.Node != "":
		fmt.Fprintf(w, "Environment %s on node %s\n", pal.Bold(r.Namespace), pal.Bold(r.Node))
	default:
		fmt.Fprintf(w, "Environment %s\n", pal.Bold(r.Namespace))
	}
	section(w, "Host", r.Host, pal)
	if r.Exists {
		section(w, "Support services", r.Support, pal)
	}
	if r.Registry != "" {
		fmt.Fprintf(w, "\nregistry %s (credentials in secret %s/registry-auth)\n", r.Registry, r.Namespace)
	}
	if len(r.VMs) > 0 {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("VMs"))
		var t ui.Table
		t.Row("  NAME", "STATUS", "READY", "NODE")
		for _, vm := range r.VMs {
			st, ready := pal.Yellow(vm.Status), pal.Red("false")
			if vm.Status == "Running" {
				st = pal.Green(vm.Status)
			}
			if vm.Ready {
				ready = pal.Green("true")
			}
			t.Row("  "+vm.Name, st, ready, vm.Node)
		}
		t.Write(w)
	}
	if n := len(r.Stages); n > 0 {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("Recent stages"))
		var t ui.Table
		for _, s := range r.Stages[max(0, n-statusStages):] {
			t.Row("  "+pal.Dim(s.Time.Local().Format("2006-01-02 15:04")), s.Name, stageStatus(pal, s.Status), s.Detail)
		}
		t.Write(w)
	}
	return nil
}

func section(w io.Writer, title string, rows []status.Component, pal ui.Palette) {
	fmt.Fprintf(w, "\n%s\n", pal.Bold(title))
	var t ui.Table
	for _, c := range rows {
		t.Row(pal.Icon(statusIcon[c.Status])+"  "+c.Name, c.Detail)
	}
	t.Write(w)
}

// stageStatus colors a recorded stage's outcome.
func stageStatus(pal ui.Palette, st state.StageStatus) string {
	switch st {
	case state.Done:
		return pal.Green(string(st))
	case state.Failed:
		return pal.Red(string(st))
	default:
		return pal.Yellow(string(st))
	}
}
