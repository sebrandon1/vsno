package cmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/snapshot"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	snTimeout time.Duration
	snYes     bool
	snDryRun  bool
)

var snapshotCmd = &cobra.Command{
	Use:     "snapshot",
	Aliases: []string{"snapshots"},
	Short:   "Keep a vSNO's disk, and create a vSNO from it without installing",
	Long: `snapshot keeps a vSNO's installed root disk as a VolumeSnapshot, and
vsno create <name> --from-snapshot brings that vSNO back from it in minutes
instead of reinstalling.

The disk carries the cluster's identity (node name, api.<name>.<domain> and its
certificates, clusterID), so a snapshot is only restored under the same vSNO
name. Snapshots are named after their vSNO.

A snapshot of a running vSNO needs the guest agent to freeze its filesystems,
which it cannot do inside a vSNO, so the vSNO is stopped for the snapshot and
started again afterwards.

vsno delete does not remove the snapshot: that is what makes a restore possible.`,
}

var snapshotCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Snapshot a vSNO's disk",
	Args:  cobra.MaximumNArgs(1),
	RunE:  trimmed(runSnapshotCreate),
}

var snapshotListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the snapshots",
	Args:    cobra.NoArgs,
	RunE:    runSnapshotList,
}

var snapshotDeleteCmd = &cobra.Command{
	Use:   "delete <name>...",
	Short: "Delete snapshots",
	Long: `delete removes the named snapshots and the installer files kept with them.
It lists what it will delete and asks you to type "yes", unless --yes.`,
	Args: someArgs("the vSNOs to snapshot, as `vsno list` shows them"),
	RunE: trimmed(runSnapshotDelete),
}

func init() {
	// On create only: the storage driver is waited for while a snapshot is taken.
	// list and delete work on what is already there.
	snapshotCreateCmd.Flags().DurationVar(&snTimeout, "timeout", 30*time.Minute, "how long to wait for the storage driver")
	f := snapshotDeleteCmd.Flags()
	f.BoolVar(&snYes, "yes", false, "don't ask for confirmation")
	f.BoolVar(&snDryRun, "dry-run", false, "list what would be deleted and change nothing")
	snapshotCmd.AddCommand(snapshotCreateCmd, snapshotListCmd, snapshotDeleteCmd)
	rootCmd.AddCommand(snapshotCmd)
}

func snapshotEnv(cmd *cobra.Command) (snapshot.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return snapshot.Env{}, err
	}
	t := snTimeout
	if t == 0 {
		t = 30 * time.Minute // list and delete carry no --timeout of their own
	}
	ge, err := guestEnv(cmd.Context(), c, t)
	return snapshot.Env{Env: ge}, err
}

func runSnapshotCreate(cmd *cobra.Command, args []string) error {
	env, err := snapshotEnv(cmd)
	if err != nil {
		return err
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	if outputFormat != formatJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Snapshotting the disk of %s in %s; it stops for the snapshot.\n\n", orDefault(name, "the only vSNO"), namespace)
	}
	res, err := snapshot.Create(cmd.Context(), env, snapshot.CreateOptions{Namespace: namespace, Name: name})
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		return writeJSON(w, res)
	}
	fmt.Fprintf(w, "\n%s Snapshot %s of %s (%s, %s).\n", pal.Icon("✓"), pal.Bold(res.Name), res.VSNO, orDash(res.Version), bytesText(res.SizeBytes))
	fmt.Fprintf(w, "Bring it back with `vsno create %s --from-snapshot`.\n", res.VSNO)
	return nil
}

func runSnapshotList(cmd *cobra.Command, _ []string) error {
	env, err := snapshotEnv(cmd)
	if err != nil {
		return err
	}
	entries, err := snapshot.List(cmd.Context(), env, namespace)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if outputFormat == formatJSON {
		if entries == nil {
			entries = []snapshot.Entry{}
		}
		return writeJSON(w, entries)
	}
	renderSnapshots(w, entries, palette(w))
	return nil
}

// renderSnapshots prints the snapshots as a table.
func renderSnapshots(w io.Writer, entries []snapshot.Entry, pal ui.Palette) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "No snapshots. `vsno snapshot create <vsno>` keeps one.")
		return
	}
	var t ui.Table
	t.Row("NAME", "VSNO", "VERSION", "SIZE", "AGE", "STATE")
	for _, e := range entries {
		// A snapshot can be restored under its vSNO's name only while that
		// vSNO is gone: say which case this is, not "in use", which reads as
		// if the snapshot were busy.
		state := pal.Green("restorable")
		if e.InUse {
			state = pal.Dim("vSNO " + e.VSNO + " exists")
		}
		t.Row(e.Name, e.VSNO, orDash(e.Version), bytesText(e.SizeBytes), ageSince(time.Now(), e.Created), state)
	}
	t.Write(w)
}

func runSnapshotDelete(cmd *cobra.Command, args []string) error {
	env, err := snapshotEnv(cmd)
	if err != nil {
		return err
	}
	ew, pal := cmd.ErrOrStderr(), palette(cmd.ErrOrStderr())
	res, err := snapshot.Delete(cmd.Context(), env, snapshot.DeleteOptions{
		Namespace: namespace, Names: args, DryRun: snDryRun, Yes: snYes,
		Confirm:  confirmOnTerminal(os.Stdin, ew),
		ShowPlan: func(d snapshot.Deleted) { renderSnapshotPlan(ew, d, pal) },
	})
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if outputFormat == formatJSON {
		return writeJSON(w, res)
	}
	switch {
	case res.Empty():
		fmt.Fprintln(w, "Nothing to delete.")
	case snDryRun:
		fmt.Fprintln(w, "Dry run: nothing was changed.")
	default:
		fmt.Fprintf(w, "%s Deleted %d snapshot(s).\n", pal.Icon("✓"), len(res.Snapshots))
	}
	return nil
}

func renderSnapshotPlan(w io.Writer, d snapshot.Deleted, pal ui.Palette) {
	if d.Empty() {
		return
	}
	fmt.Fprintf(w, "\n%s\n", pal.Bold("Snapshots to delete"))
	for _, s := range d.Snapshots {
		fmt.Fprintf(w, "  %s %s (%s, %s)\n", pal.Dim("delete"), s.Name, orDash(s.Version), bytesText(s.SizeBytes))
	}
}

// orDefault is s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// snapshotOf is the snapshot a vSNO would be restored from, for create.
func snapshotOf(cmd *cobra.Command, name string) (*snapshot.Entry, error) {
	env, err := snapshotEnv(cmd)
	if err != nil {
		return nil, err
	}
	return snapshot.Use(cmd.Context(), env, namespace, name)
}
