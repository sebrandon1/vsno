package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/isos"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	"github.com/sebrandon1/vsno/lib/status"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

// statusStages is how many recent stages the table shows; JSON has them all.
const statusStages = 5

var (
	stWatch    bool
	stInterval time.Duration
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the state of a vSNO environment",
	Long: `status reports, without changing anything:

  - the host components setup installs: the OpenShift Virtualization and
    LVM Storage operators, HyperConverged, the LVMCluster and StorageClass
  - the support services in the namespace and the registry endpoint
  - the vSNOs, with their install state (as vsno list shows it), and any
    other VMs in the namespace
  - the most recent stages recorded in ConfigMap <namespace>/vsno-state

It exits non-zero when the namespace is missing or a component is missing or
failing.`,
	Args: cobra.NoArgs,
	RunE: runStatus,
}

func init() {
	f := statusCmd.Flags()
	f.BoolVarP(&stWatch, "watch", "w", false, "refresh until interrupted")
	f.DurationVar(&stInterval, "interval", 15*time.Second, "refresh interval for --watch")
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, _ []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	if err := useEnvironment(cmd.Context(), c.Kube); err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	draw := func() error {
		fmt.Fprint(cmd.ErrOrStderr(), envNote(cmd.Context(), c.Kube, namespace))
		r, err := collectStatus(cmd, c)
		if err != nil {
			return err
		}
		if err := renderStatus(w, r, pal); err != nil {
			return err
		}
		if !r.Healthy() && !stWatch {
			return errors.New("status: the environment is not ready")
		}
		return nil
	}
	if !stWatch {
		return draw()
	}
	return watchLoop(cmd, w, pal, stInterval, draw)
}

// collectStatus is status's one read of the cluster, without rendering.
func collectStatus(cmd *cobra.Command, c *kube.Clients) (*status.Report, error) {
	r, err := status.Collect(cmd.Context(), c.Kube, c.Dyn, namespace)
	if err != nil {
		return nil, err
	}
	if r.Exists {
		env, err := guestEnv(cmd.Context(), c, 0)
		if err != nil {
			return nil, err
		}
		env.Progress = nil
		if r.VSNOs, err = guest.List(cmd.Context(), env, namespace); err != nil {
			r.Support = append(r.Support, status.Component{Name: "vsnos", Status: precheck.Warn, Detail: err.Error()})
		}
		if c := builderVolume(cmd.Context(), env); c != nil {
			r.Support = append(r.Support, *c)
		}
		// r.VSNOs are already listed: they say which releases are in use.
		users := cacheUsers(r.VSNOs)
		rep, cerr := cache.List(cmd.Context(), cache.Env{Kube: c.Kube, Dyn: c.Dyn, Config: c.Config, Builder: env.Builder}, namespace, users)
		if cerr != nil {
			r.Support = append(r.Support, status.Component{Name: "release cache", Status: precheck.Warn, Detail: cerr.Error()})
		} else {
			r.ReleaseCache = rep
		}
	}
	r.Next = status.Next(r)
	return r, nil
}

// builderVolume is the builder's volume usage, warned about above 80%. It is
// nil when the builder can't be read, so status never fails over it.
func builderVolume(ctx context.Context, env guest.Env) *status.Component {
	if env.Builder == nil {
		return nil
	}
	rep, err := isos.List(ctx, isos.Env{Env: env}, namespace)
	if err != nil || rep.Total == 0 {
		return nil
	}
	pct := float64(rep.Used) / float64(rep.Total) * 100
	st := precheck.Pass
	if pct >= 80 {
		st = precheck.Warn
	}
	return &status.Component{Name: "builder volume", Status: st,
		Detail: fmt.Sprintf("%s of %s used (%.0f%%)", bytesText(rep.Used), bytesText(rep.Total), pct)}
}

func renderStatus(w io.Writer, r *status.Report, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return cliout.PrintJSON(w, r)
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
	renderVSNOs(w, r.VSNOs, pal)
	renderVMs(w, r.VMs, pal)
	renderReleaseCache(w, r.ReleaseCache, pal)
	if r.Next != "" {
		fmt.Fprintf(w, "\n%s %s\n", pal.Bold("Next:"), r.Next)
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

// renderReleaseCache shows what the registry holds for installs.
func renderReleaseCache(w io.Writer, rep *cache.Report, pal ui.Palette) {
	if rep == nil {
		return
	}
	fmt.Fprintf(w, "\n%s\n", pal.Bold("Release cache"))
	if len(rep.Entries) == 0 {
		fmt.Fprintln(w, "  nothing cached; `vsno create` mirrors a release the first time it needs it")
		return
	}
	var t ui.Table
	t.Row("  "+pal.Bold("VERSION"), pal.Bold("SIZE"), pal.Bold("LAST USED"), pal.Bold("VSNOS"))
	for _, e := range rep.Entries {
		ver := orDash(e.Version)
		if e.StateText() != "ready" {
			ver += " (" + e.StateText() + ")"
		}
		t.Row("  "+ver, bytesText(e.SizeBytes), ageSince(time.Now(), e.LastUsed), joinItems(e.Users))
	}
	t.Write(w)
	if rep.Total > 0 {
		fmt.Fprintf(w, "  registry volume: %s of %s used, %s free\n",
			bytesText(rep.Used), bytesText(rep.Total), bytesText(rep.Total-rep.Used))
	}
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

func renderVSNOs(w io.Writer, vsnos []guest.Guest, pal ui.Palette) {
	if len(vsnos) > 0 {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("vSNOs"))
		var t ui.Table
		t.Row("  NAME", "STATE", "VERSION", "DETAIL")
		for _, g := range vsnos {
			t.Row("  "+g.Name, stateText(pal, g.State), g.ShownVersion(), pal.Dim(vsnoDetail(g)))
		}
		t.Write(w)
	}
}

// vsnoDetail is a vSNO's detail with the ClusterVersion's own message,
// which `vsno list` keeps to -o json while the rollout is fresh.
func vsnoDetail(g guest.Guest) string {
	switch {
	case g.CVMessage == "" || g.CVMessage == g.Detail:
		return g.Detail
	case g.Detail == "":
		return g.CVMessage
	}
	return g.Detail + ": " + g.CVMessage
}

func renderVMs(w io.Writer, vms []status.VM, pal ui.Palette) {
	if len(vms) > 0 {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("Other VMs"))
		var t ui.Table
		t.Row("  NAME", "STATUS", "READY", "NODE")
		for _, vm := range vms {
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
}
