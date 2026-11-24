package cmd

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/status"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	lsWatch    bool
	lsInterval time.Duration
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the vSNOs and what's left on their node",
	Long: `list shows each vSNO in the environment: its state (building-iso, importing-iso,
starting, installing, finishing, making-seed, ready, stopped, failed, abandoned), OpenShift version,
size, age and API, plus a footer with the memory left on the node and how many
more default-size vSNOs fit. Running vSNOs are asked for their state through
the builder pod. A state that is really progress — a control plane still
rolling out, a seed image being made — is named as such; the ClusterVersion's
own message is in -o json and in vsno status.`,
	Args: cobra.NoArgs,
	RunE: runList,
}

func init() {
	listCmd.Flags().BoolVarP(&lsWatch, "watch", "w", false, "refresh until interrupted")
	listCmd.Flags().DurationVar(&lsInterval, "interval", 15*time.Second, "refresh interval for --watch")
	rootCmd.AddCommand(listCmd)
}

// listing is list's report. Namespace is set only when -n picked one.
type listing struct {
	Namespace string           `json:"namespace,omitempty"`
	HostMinor string           `json:"hostMinor,omitempty"` // the host's OpenShift minor, for the empty hint
	VSNOs     []guest.Guest    `json:"vsnos"`
	Capacity  []guest.Capacity `json:"capacity,omitempty"`
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	single := rootCmd.PersistentFlags().Changed("namespace") || namespaceFromConfig
	var env guest.Env
	if single {
		if env, err = guestEnv(cmd.Context(), c, 0); err != nil {
			return err
		}
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	draw := func() error {
		l, err := collect(cmd.Context(), c, env, single)
		if err != nil {
			return err
		}
		return renderList(w, l, time.Now(), pal)
	}
	if !lsWatch {
		return draw()
	}
	return watchLoop(cmd, w, pal, lsInterval, draw)
}

// collect reads the vSNOs and the nodes' capacity, from one environment or all.
func collect(ctx context.Context, c *kube.Clients, env guest.Env, single bool) (*listing, error) {
	l := &listing{}
	var err error
	if single {
		l.Namespace = namespace
		l.VSNOs, err = guest.List(ctx, env, namespace)
	} else {
		l.VSNOs, err = guest.ListAll(ctx, c.Kube, func(ns string) (guest.Env, error) { return guestEnvFor(c, ns, 0) })
	}
	if err != nil {
		return nil, err
	}
	nodes := map[string]struct{}{}
	for _, g := range l.VSNOs {
		if g.Node != "" {
			nodes[g.Node] = struct{}{}
		}
	}
	d, _ := cluster.ReadDensity(ctx, c.Dyn) // unreadable: count without overcommit
	l.HostMinor = precheck.HostMinor(ctx, c.Dyn)
	for _, n := range slices.Sorted(maps.Keys(nodes)) {
		if capa, cerr := guest.NodeCapacity(ctx, c.Kube, n, d.Percent()); cerr == nil {
			l.Capacity = append(l.Capacity, capa)
		}
	}
	return l, nil
}

// emptyHint names the version to create a first vSNO at: the host's own minor
// when it could be read, since that is what the release cache and the default
// `vsno run` versions are built from.
func emptyHint(l *listing) string {
	v := "<minor>"
	if l.HostMinor != "" {
		v = l.HostMinor
	}
	return "Create one with `vsno create <name> --version " + v + "`."
}

func renderList(w io.Writer, l *listing, now time.Time, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return cliout.PrintJSON(w, l)
	}
	if len(l.VSNOs) == 0 {
		if l.Namespace == "" {
			fmt.Fprintln(w, "No vSNOs in any vsno environment. "+emptyHint(l))
			return nil
		}
		fmt.Fprintf(w, "No vSNOs in %s. %s\n", l.Namespace, emptyHint(l))
	} else {
		var t ui.Table
		head := []string{pal.Bold("NAMESPACE"), pal.Bold("NAME"), pal.Bold("STATE"), pal.Bold("VERSION"), pal.Bold("AGE"),
			pal.Bold("CPU"), pal.Bold("MEMORY"), pal.Bold("DISK"), pal.Bold("API"), pal.Bold("DETAIL")}
		if outputFormat == formatWide {
			head = append(head, pal.Bold("CACHED"), pal.Bold("ISO"), pal.Bold("INSTALL"), pal.Bold("REBOOT"), pal.Bold("READY"), pal.Bold("LCA"))
		}
		t.Row(head...)
		for _, g := range l.VSNOs {
			api := "-"
			if g.ServiceIP != "" {
				api = fmt.Sprintf("api.%s (%s)", g.Name, g.ServiceIP)
			}
			row := []string{g.Namespace, g.Name, stateText(pal, g.State), g.ShownVersion(), age(now.Sub(g.Created)), size(g.Cores, ""),
				size(g.MemoryGiB, "Gi"), size(g.DiskGiB, "Gi"), api, pal.Dim(detailText(g))}
			if outputFormat == formatWide {
				row = append(row, yesNo(g.Cached), orDash(g.Timings.ISO), orDash(g.Timings.Install), orDash(g.Timings.Reboot), orDash(g.Timings.Available), lcaText(g.LCA))
			}
			t.Row(row...)
		}
		t.Write(w)
	}
	for _, c := range l.Capacity {
		fmt.Fprintf(w, "\nnode %s: %d GiB memory free, room for %d more default-size vSNO(s) (%d GiB each)\n",
			c.Node, c.FreeMemoryGiB, c.FitDefault, guest.DefaultMemoryGiB)
	}
	// What the vSNOs are ready for, from the part of the report list has.
	if len(l.VSNOs) > 0 {
		if next := status.Next(&status.Report{Exists: true, VSNOs: l.VSNOs}); next != "" {
			fmt.Fprintf(w, "\n%s %s\n", pal.Bold("Next:"), next)
		}
	}
	return nil
}

// lcaText is where a vSNO's LCA came from: "catalog", or the ref it was built from.
func lcaText(src string) string {
	if src == "" {
		return "catalog"
	}
	return src
}

// detailText is a vSNO's detail, led by its IBU stage when one is under way.
func detailText(g guest.Guest) string {
	switch {
	case g.IBU == "":
		return g.Detail
	case g.Detail == "":
		return "IBU " + g.IBU
	}
	return "IBU " + g.IBU + "; " + g.Detail
}

// yesNo is "yes" or "-" for a measurement that may not have happened.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// orDash is s, or "-" when it is empty: a value create didn't measure, say.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stateText colors a state: ready green, failed red, stopped and unknown dim,
// anything in progress yellow.
func stateText(pal ui.Palette, s guest.State) string {
	switch s {
	case guest.Ready:
		return pal.Green(string(s))
	case guest.Failed, guest.Abandoned:
		return pal.Red(string(s))
	case guest.Stopped, guest.Unknown:
		return pal.Dim(string(s))
	default:
		return pal.Yellow(string(s))
	}
}

// age renders a duration like kubectl: 45s, 12m, 5h, 3d.
// ageSince is how long ago t was, or "-" for a time never set (a cache
// entry still mirroring has no last use yet).
func ageSince(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return age(now.Sub(t))
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// size renders n with unit, or "-" while unknown (a vSNO still building its ISO).
func size(n int64, unit string) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n) + unit
}
