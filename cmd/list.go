package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/state"
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
	Long: `list shows each vSNO in the environment: its state (importing-iso, starting,
installing, finishing, ready, stopped, failed), OpenShift version, size, age
and API, plus a footer with the memory left on the node and how many more
default-size vSNOs fit. Running vSNOs are asked for their state through the
builder pod.`,
	Args: cobra.NoArgs,
	RunE: runList,
}

func init() {
	listCmd.Flags().BoolVarP(&lsWatch, "watch", "w", false, "refresh until interrupted")
	listCmd.Flags().DurationVar(&lsInterval, "interval", 15*time.Second, "refresh interval for --watch")
	rootCmd.AddCommand(listCmd)
}

// listing is list's report.
type listing struct {
	Namespace string          `json:"namespace"`
	VSNOs     []guest.Guest   `json:"vsnos"`
	Capacity  *guest.Capacity `json:"capacity,omitempty"`
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	env, err := guestEnv(cmd.Context(), c, 0)
	if err != nil {
		return err
	}
	env.Progress = nil // a table, not steps
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	for {
		l := listing{Namespace: namespace}
		if l.VSNOs, err = guest.List(cmd.Context(), env, namespace); err != nil {
			return err
		}
		if st, found, _ := state.Load(cmd.Context(), c.Kube, namespace); found && st.Node != "" {
			if capa, err := guest.NodeCapacity(cmd.Context(), c.Kube, st.Node); err == nil {
				l.Capacity = &capa
			}
		}
		if lsWatch && pal.On {
			fmt.Fprint(w, "\033[H\033[2J") // clear the screen between refreshes
		}
		if err := renderList(w, &l, time.Now(), pal); err != nil || !lsWatch {
			return err
		}
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(lsInterval):
		}
	}
}

func renderList(w io.Writer, l *listing, now time.Time, pal ui.Palette) error {
	if outputFormat == formatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(l)
	}
	if len(l.VSNOs) == 0 {
		fmt.Fprintf(w, "No vSNOs in %s. Create one with `vsno create <name> --version 4.20`.\n", l.Namespace)
	} else {
		var t ui.Table
		t.Row(pal.Bold("NAME"), pal.Bold("STATE"), pal.Bold("VERSION"), pal.Bold("AGE"), pal.Bold("CPU"), pal.Bold("MEMORY"), pal.Bold("DISK"), pal.Bold("API"), pal.Bold("DETAIL"))
		for _, g := range l.VSNOs {
			api := "-"
			if g.ServiceIP != "" {
				api = fmt.Sprintf("api.%s (%s)", g.Name, g.ServiceIP)
			}
			t.Row(g.Name, stateText(pal, g.State), g.ShownVersion(), age(now.Sub(g.Created)), size(g.Cores, ""),
				size(g.MemoryGiB, "Gi"), size(g.DiskGiB, "Gi"), api, pal.Dim(g.Detail))
		}
		t.Write(w)
	}
	if c := l.Capacity; c != nil {
		fmt.Fprintf(w, "\nnode %s: %d GiB memory free, room for %d more default-size vSNO(s) (%d GiB each)\n",
			c.Node, c.FreeMemoryGiB, c.FitDefault, guest.DefaultMemoryGiB)
	}
	return nil
}

// stateText colors a state: ready green, failed red, stopped and unknown dim,
// anything in progress yellow.
func stateText(pal ui.Palette, s guest.State) string {
	switch s {
	case guest.Ready:
		return pal.Green(string(s))
	case guest.Failed:
		return pal.Red(string(s))
	case guest.Stopped, guest.Unknown:
		return pal.Dim(string(s))
	default:
		return pal.Yellow(string(s))
	}
}

// age renders a duration like kubectl: 45s, 12m, 5h, 3d.
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
