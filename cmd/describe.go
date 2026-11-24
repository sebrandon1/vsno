package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/snapshot"
	"github.com/sebrandon1/vsno/lib/status"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var describeCmd = &cobra.Command{
	Use:   "describe <name>",
	Short: "Show one vSNO in full",
	Long: `describe shows everything vsno knows about one vSNO in one place: its state
and detail (with the ClusterVersion's own message), the version it runs and
the one that was asked for, its release image, size, node, API, age, whether
it installed from the release cache, its LCA source, create timings,
unhealthy operators, the Image Based Upgrade stage, the seed images made from
it, its snapshot, and what to run next.

The seed images and the snapshot are read from the environment's registry and
builder; when either cannot be read, the rest is still shown, with the reason
as a warning. -o json carries the same document.`,
	Args:              oneArg("the vSNO, as `vsno list` shows it"),
	ValidArgsFunction: completeVSNOFirst,
	RunE:              trimmed(runDescribe),
}

func init() {
	rootCmd.AddCommand(describeCmd)
}

// vsnoDescription is describe's report: the vSNO, what was made from it, and
// what to do next.
type vsnoDescription struct {
	VSNO     guest.Guest     `json:"vsno"`
	Seeds    []seed.Image    `json:"seeds,omitempty"`
	Snapshot *snapshot.Entry `json:"snapshot,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
	Next     string          `json:"next,omitempty"`
}

func runDescribe(cmd *cobra.Command, args []string) error {
	d, err := describeVSNO(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	return renderDescribe(w, d, time.Now(), palette(w))
}

// describeVSNO reads the vSNO through the same listing vsno list uses (one
// probe batch, which also fills in the ClusterVersion message, the operator
// count and the timings), then what was made from it.
func describeVSNO(ctx context.Context, name string) (*vsnoDescription, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	env, err := guestEnv(ctx, c, 0)
	if err != nil {
		return nil, err
	}
	env.Progress = nil // one document, not a progress log
	gs, err := guest.List(ctx, env, namespace)
	if err != nil {
		return nil, err
	}
	g, err := findGuest(gs, name)
	if err != nil {
		return nil, err
	}
	d := &vsnoDescription{VSNO: *g, Next: status.NextFor(*g)}
	if imgs, err := seed.List(ctx, seed.Env{Env: env}, namespace); err != nil {
		d.Warnings = append(d.Warnings, "seed images: "+err.Error())
	} else {
		d.Seeds = seedsFrom(imgs, name)
	}
	if entries, err := snapshot.List(ctx, snapshot.Env{Env: env}, namespace); err != nil {
		d.Warnings = append(d.Warnings, "snapshots: "+err.Error())
	} else {
		d.Snapshot = snapshotFor(entries, name)
	}
	return d, nil
}

func findGuest(gs []guest.Guest, name string) (*guest.Guest, error) {
	for i := range gs {
		if gs[i].Name == name {
			return &gs[i], nil
		}
	}
	return nil, noVSNO(name, namespace)
}

// seedsFrom keeps the seed images made from name.
func seedsFrom(imgs []seed.Image, name string) []seed.Image {
	var out []seed.Image
	for _, im := range imgs {
		if im.From == name {
			out = append(out, im)
		}
	}
	return out
}

// snapshotFor is name's snapshot: snapshots are named after their vSNO, and
// List puts the newest first.
func snapshotFor(entries []snapshot.Entry, name string) *snapshot.Entry {
	for i := range entries {
		if entries[i].VSNO == name {
			return &entries[i]
		}
	}
	return nil
}

func renderDescribe(w io.Writer, d *vsnoDescription, now time.Time, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return writeJSON(w, d)
	}
	g := d.VSNO
	fmt.Fprintf(w, "%s %s %s\n", pal.Bold("vSNO"), pal.Bold(g.Name), pal.Dim("in "+g.Namespace))
	var t ui.Table
	field := func(label, value string) { t.Row("  "+pal.Dim(label), value) }
	field("state", stateText(pal, g.State))
	field("detail", orDash(pal.Dim(g.Detail)))
	if g.CVMessage != "" && g.CVMessage != g.Detail {
		field("cv message", pal.Dim(g.CVMessage))
	}
	field("version", orDash(g.ShownVersion()))
	field("requested", orDash(g.Requested))
	field("image", orDash(g.Image))
	field("size", describeSize(g))
	field("node", orDash(g.Node))
	field("api", describeAPI(g))
	field("created", describeCreated(now, g))
	field("cached", yesNo(g.Cached))
	field("lca", lcaText(g.LCA))
	field("unhealthy", unhealthyText(g.Unhealthy))
	field("ibu", orDash(g.IBU))
	field("timings", timingsText(g.Timings))
	t.Write(w)

	if len(d.Seeds) > 0 {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("Seed images"))
		var st ui.Table
		st.Row("  IMAGE", "VERSION", "SIZE", "AGE")
		for _, im := range d.Seeds {
			st.Row("  "+im.Ref, orDash(im.Version), bytesText(im.SizeBytes), seedAge(now, im))
		}
		st.Write(w)
	}
	if d.Snapshot != nil {
		fmt.Fprintf(w, "\n%s\n", pal.Bold("Snapshot"))
		renderSnapshots(w, []snapshot.Entry{*d.Snapshot}, now, pal)
	}
	for _, warn := range d.Warnings {
		fmt.Fprintf(w, "\n%s %s\n", pal.Icon("!"), warn)
	}
	if d.Next != "" {
		fmt.Fprintf(w, "\n%s %s\n", pal.Bold("Next:"), d.Next)
	}
	return nil
}

// describeSize is the vSNO's shape, skipping whatever it has not reported yet.
func describeSize(g guest.Guest) string {
	var parts []string
	if g.Cores > 0 {
		parts = append(parts, fmt.Sprintf("%d vCPU", g.Cores))
	}
	if g.MemoryGiB > 0 {
		parts = append(parts, fmt.Sprintf("%d GiB memory", g.MemoryGiB))
	}
	if g.DiskGiB > 0 {
		parts = append(parts, fmt.Sprintf("%d GiB disk", g.DiskGiB))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " / ")
}

func describeAPI(g guest.Guest) string {
	if g.ServiceIP == "" {
		return orDash(g.API)
	}
	return fmt.Sprintf("%s (%s)", g.API, g.ServiceIP)
}

func describeCreated(now time.Time, g guest.Guest) string {
	if g.Created.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s (%s)", g.Created.Format(time.RFC3339), ageSince(now, g.Created))
}

func unhealthyText(n *int) string {
	switch {
	case n == nil:
		return "-"
	case *n == 0:
		return "0 operators"
	}
	return fmt.Sprintf("%d operator(s)", *n)
}

// timingsText is what create measured, or "-" when it did not run with --watch.
func timingsText(t guest.Timings) string {
	if t.Empty() {
		return "-"
	}
	var parts []string
	for _, p := range []struct{ name, v string }{
		{"iso", t.ISO}, {"install", t.Install}, {"reboot", t.Reboot}, {"available", t.Available},
	} {
		if p.v != "" {
			parts = append(parts, p.name+" "+p.v)
		}
	}
	return strings.Join(parts, ", ")
}
