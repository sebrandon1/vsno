package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/setup"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/yaml"
)

var (
	suNode    string
	suDisk    string
	suConfig  string
	suYes     bool
	suDryRun  bool
	suNoProbe bool
	suTimeout time.Duration
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Install what vSNOs need on the host cluster",
	Long: `setup prepares the host cluster for vSNO VMs:

  - OpenShift Virtualization and LVM Storage (reused if already installed)
  - an LVMCluster on the chosen worker's disk, unless StorageClass
    lvms-vg-vsno already serves that worker
  - the vsno namespace with DNS (dnsmasq), a builder pod, S3 (SeaweedFS) and
    a TLS registry with generated credentials

It runs precheck first. Cluster-level failures stop setup; CPU and memory
shortfalls on the node only warn. Settings come from flags, then --config (the
file 'vsno precheck --write-config' writes), then the precheck's suggestion.

Taking over a disk asks you to type the node name, unless --yes. Reruns reuse
generated credentials and report "unchanged" for objects that didn't change.`,
	RunE: runSetup,
}

func init() {
	f := setupCmd.Flags()
	f.StringVar(&suNode, "node", "", "worker that will run the VMs (default: precheck's suggestion)")
	f.StringVar(&suDisk, "disk", "", "disk for LVMS, a /dev/disk/by-id path (default: precheck's suggestion)")
	f.StringVar(&suConfig, "config", "", "settings file written by 'vsno precheck --write-config'")
	f.BoolVar(&suYes, "yes", false, "approve taking over the LVMS disk without asking")
	f.BoolVar(&suDryRun, "dry-run", false, "print the manifests setup would apply and change nothing")
	f.BoolVar(&suNoProbe, "no-probe", false, "skip the precheck hardware probe (then --node is required)")
	f.DurationVar(&suTimeout, "timeout", 20*time.Minute, "how long to wait for each component")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, _ []string) error {
	settings, err := setupSettings()
	if err != nil {
		return err
	}
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(c.Config)
	if err != nil {
		return err
	}
	progress := precheckProgress{newProgress()}
	env := setup.Env{
		Kube: c.Kube, Dyn: c.Dyn, Applier: apply.NewSSA(dc, c.Dyn),
		Precheck: func(ctx context.Context, node string) *precheck.Report {
			pe := &precheck.Env{Kube: c.Kube, Dyn: c.Dyn, Namespace: namespace, Progress: progress}
			if !suNoProbe {
				pe.Prober = &precheck.PodProber{Kube: c.Kube, Image: precheck.DefaultProbeImage, Timeout: 3 * time.Minute}
			}
			return precheck.Run(ctx, pe, precheck.DefaultRequirements(), precheck.Options{Node: node})
		},
		Confirm:  confirmOnTerminal(os.Stdin, cmd.ErrOrStderr()),
		Progress: progress,
		Timeout:  suTimeout,
	}
	if outputFormat == "table" {
		fmt.Fprintf(cmd.ErrOrStderr(), "Setting up %s in %s (context %q)...\n\n", namespace, c.Config.Host, c.Context)
	}
	res, err := setup.Run(cmd.Context(), env, setup.Options{
		Namespace: namespace, Settings: settings, Yes: suYes, DryRun: suDryRun, Images: manifests.DefaultImages(),
	})
	if outputFormat == "table" {
		fmt.Fprintln(cmd.ErrOrStderr())
	}
	if rerr := renderSetup(cmd.OutOrStdout(), res, suDryRun, palette(cmd.OutOrStdout())); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// setupSettings reads --config and applies --node/--disk over it.
func setupSettings() (precheck.Suggested, error) {
	var s precheck.Suggested
	if suConfig != "" {
		b, err := os.ReadFile(suConfig) // #nosec G304 -- the user names the file with --config
		if err != nil {
			return s, err
		}
		if err := yaml.UnmarshalStrict(b, &s); err != nil {
			return s, fmt.Errorf("parsing %s: %w", suConfig, err)
		}
	}
	if suNode != "" && suNode != s.Node {
		s.Node, s.Disk, s.DiskSizeGB = suNode, "", 0 // the config's disk belongs to its node
	}
	if suDisk != "" {
		s.Disk = suDisk
	}
	return s, nil
}

// confirmOnTerminal asks on w and reads the answer from in. It never approves
// when in is not a terminal, so scripts must pass --yes.
func confirmOnTerminal(in *os.File, w io.Writer) func(question, answer string) bool {
	return func(question, answer string) bool {
		if !term.IsTerminal(int(in.Fd())) { // #nosec G115 -- fd fits in int
			fmt.Fprintln(w, question+": no terminal to ask on")
			return false
		}
		return confirm(in, w, question, answer)
	}
}

func confirm(in io.Reader, w io.Writer, question, answer string) bool {
	fmt.Fprintf(w, "%s [%s]: ", question, answer)
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.TrimSpace(line) == answer
}

// outcomeIcon marks a setup step: = reused, ✓ applied, ~ planned (dry run).
func outcomeIcon(pal ui.Palette, o setup.Outcome) string {
	switch o {
	case setup.Reused:
		return pal.Dim("=")
	case setup.Applied:
		return pal.Green("✓")
	default:
		return pal.Yellow("~")
	}
}

// renderSetup prints the setup report, or the dry-run manifests.
func renderSetup(w io.Writer, res *setup.Result, dryRun bool, pal ui.Palette) error {
	if res == nil {
		return nil
	}
	if outputFormat == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	if dryRun {
		for _, o := range res.Objects {
			b, err := yaml.Marshal(o.Object)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "---\n%s", b)
		}
		fmt.Fprintln(w)
	}
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "%s %s\n", pal.Icon("!"), warn)
	}
	var t ui.Table
	for _, s := range res.Steps {
		t.Row(outcomeIcon(pal, s.Outcome), s.Name, string(s.Outcome), s.Detail)
	}
	t.Write(w)
	if len(res.Steps) == 0 {
		return nil
	}
	ch := res.Changes
	if dryRun {
		fmt.Fprintf(w, "\nnode %s: %d object(s) would be applied; dry run, nothing was changed\n", res.Settings.Node, len(ch))
		return nil
	}
	fmt.Fprintf(w, "\nnode %s, %d object(s): %d created, %d updated, %d unchanged\n", res.Settings.Node, len(ch),
		apply.Count(ch, apply.Created), apply.Count(ch, apply.Updated), apply.Count(ch, apply.Unchanged))
	if res.Registry != "" {
		fmt.Fprintf(w, "registry %s (credentials in secret %s/registry-auth, CA in %s/registry-ca)\n", res.Registry, res.Namespace, res.Namespace)
	}
	return nil
}
