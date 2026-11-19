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
	"github.com/sebrandon1/vsno/lib/teardown"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/yaml"
)

var (
	suNode      string
	suDisk      string
	suConfig    string
	suYes       bool
	suForce     bool
	suDryRun    bool
	suManifests bool
	suNoProbe   bool
	suTimeout   time.Duration
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
generated credentials and report "unchanged" for objects that didn't change.

setup asks before taking over an existing namespace it didn't create (no
app.kubernetes.io/managed-by=vsno label and no vsno-state ConfigMap), because it
would overwrite objects there; --force takes it over without asking. A
namespace that holds vSNOs isn't even asked about: setup would fight the VMs in
it, so only --force takes it over.

--dry-run lists what setup would do and changes nothing: each step, and each
object it would create, reapply (vsno applied it before) or take over (it
exists but vsno didn't create it, so setup would overwrite it). Add
--manifests to also print the manifests, with Secret values redacted.`,
	RunE: runSetup,
}

func init() {
	f := setupCmd.Flags()
	f.StringVar(&suNode, "node", "", "worker that will run the VMs (default: precheck's suggestion)")
	f.StringVar(&suDisk, "disk", "", "disk for LVMS, a /dev/disk/by-id path (default: precheck's suggestion)")
	f.StringVar(&suConfig, "config", "", "settings file written by 'vsno precheck --write-config'")
	f.BoolVar(&suYes, "yes", false, "approve taking over the LVMS disk without asking")
	f.BoolVar(&suForce, "force", false, "take over an existing namespace vsno didn't create without asking, overwriting objects in it")
	f.BoolVar(&suDryRun, "dry-run", false, "list what setup would do and change nothing")
	f.BoolVar(&suManifests, "manifests", false, "with --dry-run, also print the manifests (Secret values redacted); implies --dry-run")
	f.BoolVar(&suNoProbe, "no-probe", false, "skip the precheck hardware probe (then --node is required)")
	f.DurationVar(&suTimeout, "timeout", 20*time.Minute, "how long to wait for each component")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, _ []string) error {
	if suManifests {
		suDryRun = true
	}
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
	if outputFormat == formatTable {
		fmt.Fprintf(cmd.ErrOrStderr(), "Setting up %s in %s (context %q)...\n\n", namespace, c.Config.Host, c.Context)
	}
	res, err := setup.Run(cmd.Context(), env, setup.Options{
		Namespace: namespace, Settings: settings, Yes: suYes, Force: suForce, DryRun: suDryRun, Images: manifests.DefaultImages(),
	})
	if outputFormat == formatTable {
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

// stepIcon marks a setup or teardown step: = left alone (reused, kept,
// already gone), ✓ done (applied, deleted), ~ planned.
func stepIcon(pal ui.Palette, outcome string) string {
	switch outcome {
	case string(setup.Reused), string(teardown.Kept), string(teardown.Gone):
		return pal.Dim("=")
	case string(setup.Applied), string(teardown.Deleted):
		return pal.Green("✓")
	default:
		return pal.Yellow("~")
	}
}

// renderSteps prints the step table of setup or teardown.
func renderSteps(w io.Writer, pal ui.Palette, rows [][3]string) {
	var t ui.Table
	for _, r := range rows {
		t.Row(stepIcon(pal, r[1]), r[0], r[1], r[2])
	}
	t.Write(w)
}

// renderSetup prints the setup report, or the dry-run manifests.
func renderSetup(w io.Writer, res *setup.Result, dryRun bool, pal ui.Palette) error {
	if res == nil {
		return nil
	}
	if outputFormat == formatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	if dryRun && suManifests {
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
	rows := make([][3]string, 0, len(res.Steps))
	for _, s := range res.Steps {
		rows = append(rows, [3]string{s.Name, string(s.Outcome), s.Detail})
	}
	renderSteps(w, pal, rows)
	if len(res.Steps) == 0 {
		return nil
	}
	if dryRun {
		renderPlan(w, res, pal)
		return nil
	}
	ch := res.Changes
	fmt.Fprintf(w, "\nnode %s, %d object(s): %d created, %d updated, %d unchanged\n", res.Settings.Node, len(ch),
		apply.Count(ch, apply.Created), apply.Count(ch, apply.Updated), apply.Count(ch, apply.Unchanged))
	if res.Registry != "" {
		fmt.Fprintf(w, "registry %s (credentials in secret %s/registry-auth, CA in %s/registry-ca)\n", res.Registry, res.Namespace, res.Namespace)
	}
	return nil
}

// renderPlan lists each object a dry run would apply and what would happen
// to it, and warns about objects setup would take over.
func renderPlan(w io.Writer, res *setup.Result, pal ui.Palette) {
	renderObjects(w, res.Changes, pal)
	fmt.Fprintf(w, "\nnode %s, %d object(s): %s. Dry run: nothing was changed.\n", res.Settings.Node, len(res.Changes), setup.PlanDetail(res.Changes))
	if n := apply.Count(res.Changes, apply.TakeOver); n > 0 {
		fmt.Fprintf(w, "%s %s\n", pal.Icon("!"), pal.Yellow(fmt.Sprintf(
			"%d object(s) exist but were not created by vsno; setup would overwrite them. To keep them, use another namespace (-n).", n)))
	}
}

// renderObjects lists objects with what setup or teardown does to them.
func renderObjects(w io.Writer, changes []apply.Change, pal ui.Palette) {
	fmt.Fprintf(w, "\n%s\n", pal.Bold("Objects"))
	var t ui.Table
	for _, c := range changes {
		name := c.Name
		if c.Namespace != "" {
			name = c.Namespace + "/" + c.Name
		}
		t.Row("  "+planAction(pal, c.Action), c.Kind, name)
	}
	t.Write(w)
}

// planAction colors an object's action: create and deleted green, delete
// red, take over yellow, reapply and absent dim.
func planAction(pal ui.Palette, a apply.Action) string {
	switch a {
	case apply.Create, apply.Removed:
		return pal.Green(string(a))
	case apply.Delete:
		return pal.Red(string(a))
	case apply.TakeOver:
		return pal.Yellow(string(a))
	default:
		return pal.Dim(string(a))
	}
}
