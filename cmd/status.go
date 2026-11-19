package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/status"
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
	if err := renderStatus(cmd.OutOrStdout(), r); err != nil {
		return err
	}
	if !r.Healthy() {
		return errors.New("status: the environment is not ready")
	}
	return nil
}

func renderStatus(w io.Writer, r *status.Report) error {
	if outputFormat == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	switch {
	case !r.Exists:
		fmt.Fprintf(w, "Namespace %s does not exist; run `vsno setup -n %s`.\n", r.Namespace, r.Namespace)
	case !r.Managed:
		fmt.Fprintf(w, "Namespace %s was not created by vsno.\n", r.Namespace)
	case r.Node != "":
		fmt.Fprintf(w, "Environment %s on node %s\n", r.Namespace, r.Node)
	default:
		fmt.Fprintf(w, "Environment %s\n", r.Namespace)
	}
	section(w, "Host", r.Host)
	if r.Exists {
		section(w, "Support services", r.Support)
	}
	if r.Registry != "" {
		fmt.Fprintf(w, "\nregistry %s (credentials in secret %s/registry-auth)\n", r.Registry, r.Namespace)
	}
	if len(r.VMs) > 0 {
		fmt.Fprintln(w, "\nVMs")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tSTATUS\tREADY\tNODE")
		for _, vm := range r.VMs {
			fmt.Fprintf(tw, "  %s\t%s\t%v\t%s\n", vm.Name, vm.Status, vm.Ready, vm.Node)
		}
		_ = tw.Flush()
	}
	if n := len(r.Stages); n > 0 {
		fmt.Fprintln(w, "\nRecent stages")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, s := range r.Stages[max(0, n-statusStages):] {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.Time.Local().Format("2006-01-02 15:04"), s.Name, s.Status, s.Detail)
		}
		_ = tw.Flush()
	}
	return nil
}

func section(w io.Writer, title string, rows []status.Component) {
	fmt.Fprintf(w, "\n%s\n", title)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range rows {
		fmt.Fprintf(tw, "%s  %s\t%s\n", statusIcon[c.Status], c.Name, c.Detail)
	}
	_ = tw.Flush()
}
