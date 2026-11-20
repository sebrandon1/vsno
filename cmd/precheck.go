package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/ui"
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

	if outputFormat == formatTable {
		fmt.Fprintf(cmd.ErrOrStderr(), "Checking %s (context %q)...\n\n", c.Config.Host, c.Context)
	}
	report := precheck.Run(cmd.Context(), env, pcReq, precheck.Options{Node: pcNode})
	if outputFormat == formatTable {
		fmt.Fprintln(cmd.ErrOrStderr())
	}

	out := cmd.OutOrStdout()
	if outputFormat == formatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderReport(out, report, palette(out))
	}

	if pcWriteConfig != "" {
		b, err := yaml.Marshal(report.Suggested)
		if err != nil {
			return err
		}
		if err := os.WriteFile(pcWriteConfig, b, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", pcWriteConfig, err)
		}
		if outputFormat == formatTable {
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

// reportSection is one group of results, with the title it prints under.
type reportSection struct {
	group precheck.Group
	title string
}

// reportSections are the precheck table's sections, in order.
var reportSections = []reportSection{
	{precheck.GroupCluster, "Cluster"},
	{precheck.GroupInstall, "Operators, storage and registry"},
	{precheck.GroupWorkers, "Workers"},
}

// renderSections prints the results of each section, leaving out empty ones.
// nodes and chosen are for the per-worker headings; the seed precheck has
// neither.
func renderSections(w io.Writer, results []precheck.Result, nodes []precheck.NodeSummary, chosen string, sections []reportSection, pal ui.Palette) {
	printed := false
	for _, sec := range sections {
		var rows []precheck.Result
		for _, res := range results {
			if res.Group == sec.group {
				rows = append(rows, res)
			}
		}
		if len(rows) == 0 {
			continue
		}
		if printed {
			fmt.Fprintln(w)
		}
		printed = true
		fmt.Fprintln(w, pal.Bold(sec.title))
		renderRows(w, rows, nodes, chosen, pal)
	}
}

// renderReport prints the results by section, each worker under its own
// heading, then the suggested settings.
func renderReport(w io.Writer, r *precheck.Report, pal ui.Palette) {
	renderSections(w, r.Results, r.Nodes, r.Suggested.Node, reportSections, pal)

	s := r.Suggested
	switch {
	case s.Node == "" && r.Failed() > 0:
		fmt.Fprintf(w, "\n%s\n", pal.Red(fmt.Sprintf("Result: this cluster cannot host vSNOs yet (%d failing check(s)).", r.Failed())))
		return
	case s.Node == "":
		fmt.Fprintln(w, "\nNo node suggested: the hardware probe was skipped. Run without --no-probe, or pass --node to `vsno setup`.")
		return
	}
	fmt.Fprintf(w, "\n%s\n", pal.Bold("Suggested settings for `vsno setup`:"))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
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

// renderRows prints rows in order. Consecutive rows for the same node go in a
// block under a heading with the node's verdict.
func renderRows(w io.Writer, rows []precheck.Result, nodes []precheck.NodeSummary, chosen string, pal ui.Palette) {
	for len(rows) > 0 {
		n := 1
		for n < len(rows) && rows[n].Node == rows[0].Node {
			n++
		}
		block := rows[:n]
		rows = rows[n:]
		if block[0].Node == "" {
			renderTable(w, "", block, false, pal)
			continue
		}
		icon, verdict := nodeVerdict(block, nodes, chosen)
		fmt.Fprintf(w, "%s  %s  %s\n", pal.Icon(icon), pal.Bold(block[0].Node), verdict)
		renderTable(w, "   ", block, verdict == "not usable", pal)
	}
}

// renderTable prints icon, check and detail columns, plus the hints of
// warnings and failures. Info details are dimmed. notUsable drops the
// "not usable: " prefix the node heading already states.
func renderTable(w io.Writer, indent string, rows []precheck.Result, notUsable bool, pal ui.Palette) {
	var t ui.Table
	for _, res := range rows {
		detail := res.Detail
		if notUsable {
			detail = strings.TrimPrefix(detail, "not usable: ")
		}
		if res.Status == precheck.Info {
			detail = pal.Dim(detail)
		}
		t.Row(indent+pal.Icon(statusIcon[res.Status]), res.Check, detail)
		if res.Hint != "" && (res.Status == precheck.Fail || res.Status == precheck.Warn) {
			t.Row(indent, "", pal.Yellow("→")+" "+res.Hint)
		}
	}
	t.Write(w)
}

// nodeVerdict is the icon and word for a node's heading. A node that wasn't
// evaluated (its probe failed) takes the worst status of its rows.
func nodeVerdict(block []precheck.Result, nodes []precheck.NodeSummary, chosen string) (string, string) {
	node := block[0].Node
	idx := slices.IndexFunc(nodes, func(n precheck.NodeSummary) bool { return n.Name == node })
	switch {
	case idx < 0:
		worst := precheck.Info
		for _, res := range block {
			if res.Status == precheck.Fail || (res.Status == precheck.Warn && worst != precheck.Fail) {
				worst = res.Status
			}
		}
		return statusIcon[worst], "not evaluated"
	case node == chosen:
		return statusIcon[precheck.Pass], "eligible, suggested"
	case nodes[idx].Eligible:
		return statusIcon[precheck.Pass], "eligible"
	case chosen != "":
		// Another node will be used, so this one's failures are only context.
		return statusIcon[precheck.Info], "not usable"
	default:
		return statusIcon[precheck.Fail], "not usable"
	}
}
