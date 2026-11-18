package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

var (
	pcReq         = precheck.DefaultRequirements()
	pcNode        string
	pcNoProbe     bool
	pcProbeImage  string
	pcProbeWait   time.Duration
	pcWriteConfig string
)

var precheckCmd = &cobra.Command{
	Use:   "precheck",
	Short: "Check whether this cluster can host vSNO VMs, and suggest settings",
	Long: `precheck inspects the host cluster from your kubeconfig and reports whether it
can run two vSNO VMs for an Image Based Upgrade:

  - you are cluster-admin, the cluster is OpenShift, guest networks don't overlap
  - OpenShift Virtualization and LVM Storage are in the catalog
  - a worker has /dev/kvm (ideally bare metal), enough CPU and memory, and an
    empty disk for LVMS

The hardware probe runs a short-lived privileged pod on each candidate worker in
a temporary namespace (like 'oc debug node'), then deletes it. Nothing else on
the cluster is changed. Use --no-probe to skip it.

Exits non-zero if any check fails.`,
	RunE: runPrecheck,
}

func init() {
	f := precheckCmd.Flags()
	f.StringVar(&pcReq.SeedRepo, "seed-repo", "", "registry repo for the seed image (e.g. quay.io/me/ibu-seed); checks local push credentials")
	f.IntVar(&pcReq.VMs, "vms", pcReq.VMs, "VMs that must run at the same time (seed + target)")
	f.IntVar(&pcReq.VMCores, "vm-cores", pcReq.VMCores, "vCPUs per VM")
	f.Int64Var(&pcReq.VMMemGiB, "vm-memory", pcReq.VMMemGiB, "guest memory per VM, GiB")
	f.Int64Var(&pcReq.MinDiskGB, "min-disk", pcReq.MinDiskGB, "minimum size of the LVMS disk, GB")
	f.StringVar(&pcNode, "node", "", "only probe this worker node")
	f.BoolVar(&pcNoProbe, "no-probe", false, "skip the on-node hardware probe")
	f.StringVar(&pcProbeImage, "probe-image", precheck.DefaultProbeImage, "image for the probe pod (needs bash + chroot)")
	f.DurationVar(&pcProbeWait, "probe-timeout", 3*time.Minute, "how long to wait for each probe pod")
	f.StringVar(&pcWriteConfig, "write-config", "", "write the suggested settings to this YAML file")
	rootCmd.AddCommand(precheckCmd)
}

func runPrecheck(cmd *cobra.Command, _ []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	env := &precheck.Env{
		Kube: c.Kube, Dyn: c.Dyn, Namespace: namespace,
		AuthFiles: precheck.DefaultAuthFiles(),
	}
	if !pcNoProbe {
		env.Prober = &precheck.PodProber{Kube: c.Kube, Image: pcProbeImage, Timeout: pcProbeWait}
	}
	env.Progress = precheckProgress{newProgress()}

	if outputFormat == "table" {
		fmt.Fprintf(cmd.ErrOrStderr(), "Checking %s (context %q)...\n\n", c.Config.Host, c.Context)
	}
	report := precheck.Run(cmd.Context(), env, pcReq, precheck.Options{Node: pcNode})
	if outputFormat == "table" {
		fmt.Fprintln(cmd.ErrOrStderr())
	}

	out := cmd.OutOrStdout()
	if outputFormat == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderReport(out, report)
	}

	if pcWriteConfig != "" {
		b, err := yaml.Marshal(report.Suggested)
		if err != nil {
			return err
		}
		if err := os.WriteFile(pcWriteConfig, b, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", pcWriteConfig, err)
		}
		if outputFormat == "table" {
			fmt.Fprintf(out, "\nWrote suggested settings to %s\n", pcWriteConfig)
		}
	}

	if n := report.Failed(); n > 0 {
		return fmt.Errorf("precheck: %d check(s) failed", n)
	}
	return nil
}

var statusIcon = map[precheck.Status]string{
	precheck.Pass: "✓", precheck.Warn: "!", precheck.Fail: "✗", precheck.Info: "i",
}

// renderReport prints the results table, hints and the suggested settings.
func renderReport(w io.Writer, r *precheck.Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, res := range r.Results {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", statusIcon[res.Status], res.Check, res.Detail)
		if res.Hint != "" && (res.Status == precheck.Fail || res.Status == precheck.Warn) {
			fmt.Fprintf(tw, "\t\t→ %s\n", res.Hint)
		}
	}
	_ = tw.Flush()

	s := r.Suggested
	if s.Node == "" {
		fmt.Fprintf(w, "\nResult: this cluster cannot host vSNOs yet (%d failing check(s)).\n", r.Failed())
		return
	}
	fmt.Fprintln(w, "\nSuggested settings for `vsno setup`:")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  node\t%s\n", s.Node)
	if s.Disk != "" {
		fmt.Fprintf(tw, "  disk\t%s (%d GB)\n", s.Disk, s.DiskSizeGB)
	} else {
		fmt.Fprintf(tw, "  disk\t(reuse existing LVMS storage)\n")
	}
	fmt.Fprintf(tw, "  host DNS\t%s\n", s.HostDNS)
	fmt.Fprintf(tw, "  LVMS channel\t%s\n", s.LVMSChannel)
	fmt.Fprintf(tw, "  CNV channel\t%s\n", s.CNVChannel)
	_ = tw.Flush()
	if n := r.Failed(); n > 0 {
		fmt.Fprintf(w, "\nNote: %d other check(s) failed; see above.\n", n)
	}
}
