package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/isos"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	isoKeep   int
	isoYes    bool
	isoDryRun bool
	idlYes    bool
	idlDryRun bool
)

var isoCmd = &cobra.Command{
	Use:     "iso",
	Aliases: []string{"isos"},
	Short:   "Show, delete and clean up what vSNO creates leave in the builder and on LVMS",
	Long: `iso lists what the environment holds beyond its vSNOs: the ISOs the builder
serves, the installer's build directories, the cached openshift-install
binaries (one per release) and the ISO DataVolumes, with what still uses each.
It ends with the builder volume's usage.

iso info shows one of them by name; iso prune removes everything no vSNO uses;
iso delete removes the ones you name; iso eject takes an install ISO off a vSNO
that has finished installing.`,
	Args: cobra.NoArgs,
	RunE: runIsoList,
}

var isoInfoCmd = &cobra.Command{
	Use:   "info <vsno|installer|DataVolume>",
	Short: "Show what one vSNO, installer or DataVolume holds, and who uses it",
	Long: `info shows one thing by name: its path in the builder, size and age, the
release an installer came from, its ISO DataVolume, who still uses it, and the
builder volume's usage. A vSNO brings its served ISO, its build directory and
its DataVolume; an installer or a DataVolume brings itself.

It only reads. ` + "`vsno iso list`" + ` shows everything at once.`,
	Args: cobra.ExactArgs(1),
	RunE: runIsoInfo,
}

var isoListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the ISOs, installer caches and ISO DataVolumes, and what uses them",
	Args:    cobra.NoArgs,
	RunE:    runIsoList,
}

var isoDeleteCmd = &cobra.Command{
	Use:   "delete <vsno|installer|DataVolume>...",
	Short: "Delete the ISOs, installers or ISO DataVolumes you name",
	Long: `delete removes what you name: a vSNO's served ISO and build directory, a
cached openshift-install binary, or an ISO DataVolume. Anything a vSNO still
uses is refused — ` + "`vsno iso eject`" + ` takes an install ISO off the VM first —
and it asks "are you sure?" unless --yes. --dry-run only lists.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runIsoDelete,
}

var isoPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove the ISOs, installers and ISO DataVolumes no vSNO uses",
	Long: `prune removes what a finished or failed create left behind: the served ISO and
build directory of vSNOs that no longer exist, the openshift-install binaries
no vSNO runs (--keep N keeps the N newest), and their ISO DataVolumes. It
lists them and asks you to type "prune" first, unless --yes. --dry-run only
lists.`,
	Args: cobra.NoArgs,
	RunE: runIsoPrune,
}

var isoEjectCmd = &cobra.Command{
	Use:   "eject <vsno>",
	Short: "Remove a vSNO's install ISO, which it no longer boots from",
	Long: `eject removes the install ISO from a vSNO that has finished installing: the
CD-ROM and its volume from the VM, the ISO DataVolume, and the builder's copy.
create does this itself when the install finishes; eject is for vSNOs it
didn't, such as ones from an older vsno.`,
	Args: cobra.ExactArgs(1),
	RunE: runIsoEject,
}

func init() {
	isoPruneCmd.Flags().IntVar(&isoKeep, "keep", 0, "keep the N newest unused installers")
	isoPruneCmd.Flags().BoolVar(&isoYes, "yes", false, "don't ask for confirmation")
	isoPruneCmd.Flags().BoolVar(&isoDryRun, "dry-run", false, "list what would be removed and change nothing")
	isoDeleteCmd.Flags().BoolVar(&idlYes, "yes", false, "don't ask for confirmation")
	isoDeleteCmd.Flags().BoolVar(&idlDryRun, "dry-run", false, "list what would be removed and change nothing")
	isoCmd.AddCommand(isoListCmd, isoInfoCmd, isoDeleteCmd, isoPruneCmd, isoEjectCmd)
	rootCmd.AddCommand(isoCmd)
}

func isosEnv(cmd *cobra.Command) (isos.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return isos.Env{}, err
	}
	if err := useEnvironment(cmd.Context(), c.Kube); err != nil {
		return isos.Env{}, err
	}
	e, err := guestEnv(cmd.Context(), c, 0)
	return isos.Env{Env: e}, err
}

func runIsoList(cmd *cobra.Command, _ []string) error {
	env, err := isosEnv(cmd)
	if err != nil {
		return err
	}
	rep, err := isos.List(cmd.Context(), env, namespace)
	if err != nil {
		return err
	}
	return renderIsos(cmd.OutOrStdout(), rep, palette(cmd.OutOrStdout()))
}

func runIsoInfo(cmd *cobra.Command, args []string) error {
	env, err := isosEnv(cmd)
	if err != nil {
		return err
	}
	rep, err := isos.List(cmd.Context(), env, namespace)
	if err != nil {
		return err
	}
	in, err := isos.Find(rep, args[0])
	if err != nil {
		return err
	}
	return renderIsoInfo(cmd.OutOrStdout(), in, palette(cmd.OutOrStdout()))
}

// renderIsoInfo shows one name's builder files, its DataVolumes and the volume
// they sit on.
func renderIsoInfo(w io.Writer, in *isos.Info, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return writeJSON(w, in)
	}
	line := fmt.Sprintf("%s: %s in the builder", pal.Bold(in.Name), bytesText(in.Bytes-in.DiskBytes()))
	if d := in.DiskBytes(); d > 0 {
		line += ", " + bytesText(d) + " on LVMS"
	}
	fmt.Fprintf(w, "%s\n\n", line)
	var t ui.Table
	t.Row(pal.Bold("WHAT"), pal.Bold("PATH"), pal.Bold("VERSION"), pal.Bold("SIZE"), pal.Bold("AGE"), pal.Bold("USE"))
	now := time.Now()
	var releases []string
	for _, e := range in.Entries {
		t.Row(string(e.Kind), orDash(e.Path), orDash(e.Version), bytesText(e.Size), ageSince(now, e.Modified), useText(e.Use, e.Users))
		if e.Release != "" {
			releases = append(releases, e.Release)
		}
	}
	for _, d := range in.Disks {
		t.Row("data volume", orDash(d.Name), "-", bytesText(d.Size), "-", useText(d.Use, d.Users))
	}
	t.Write(w)
	if len(releases) > 0 {
		fmt.Fprintf(w, "\n%s\n", pal.Dim("release: "+strings.Join(releases, ", ")))
	}
	writeVolume(w, in.Used, in.Total, pal)
	return nil
}

func renderIsos(w io.Writer, rep *isos.Report, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return writeJSON(w, rep)
	}
	if len(rep.Entries) == 0 && len(rep.Disks) == 0 {
		fmt.Fprintf(w, "Nothing in %s: no ISOs, installer caches or ISO DataVolumes.\n", namespace)
		return nil
	}
	var t ui.Table
	t.Row(pal.Bold("WHAT"), pal.Bold("NAME"), pal.Bold("VERSION"), pal.Bold("SIZE"), pal.Bold("AGE"), pal.Bold("USE"))
	now := time.Now()
	for _, e := range rep.Entries {
		t.Row(string(e.Kind), e.Name, orDash(e.Version), bytesText(e.Size), ageSince(now, e.Modified), useText(e.Use, e.Users))
	}
	for _, d := range rep.Disks {
		t.Row("data volume", d.Name, "-", bytesText(d.Size), "-", useText(d.Use, d.Users))
	}
	t.Write(w)
	writeVolume(w, rep.Used, rep.Total, pal)
	if rep.DiskBytes > 0 {
		fmt.Fprintf(w, "ISO DataVolumes on LVMS: %s\n", bytesText(rep.DiskBytes))
	}
	return nil
}

// writeVolume prints the builder volume's usage, and warns above 80%.
func writeVolume(w io.Writer, used, total int64, pal ui.Palette) {
	if total == 0 {
		return
	}
	pct := float64(used) / float64(total) * 100
	line := fmt.Sprintf("\nBuilder volume: %s of %s used (%.0f%%), %s free", bytesText(used), bytesText(total), pct, bytesText(total-used))
	if pct >= 80 {
		line = pal.Yellow(line)
	}
	fmt.Fprintln(w, line)
}

// useText says what still uses an entry, or "-" when nothing tracks it.
func useText(u isos.Use, users []string) string {
	switch {
	case u == "":
		return "-"
	case len(users) > 0 && u == isos.InUse:
		return string(u) + " (" + users[0] + ")"
	}
	return string(u)
}

func runIsoPrune(cmd *cobra.Command, _ []string) error {
	env, err := isosEnv(cmd)
	if err != nil {
		return err
	}
	ew := cmd.ErrOrStderr()
	shown := false
	res, err := isos.Prune(cmd.Context(), env, isos.PruneOptions{
		Namespace: namespace, DryRun: isoDryRun, Yes: isoYes, Keep: isoKeep,
		Confirm: confirmOnTerminal(os.Stdin, ew),
		ShowPlan: func(p *isos.Plan) {
			renderPrunePlan(ew, p, palette(ew))
			fmt.Fprintln(ew)
			shown = true
		},
	})
	if err != nil { // a refused prune has no result to render
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if rerr := writeJSON(w, res); rerr != nil && err == nil {
			err = rerr
		}
		return err
	}
	switch {
	case len(res.Removed) > 0:
		fmt.Fprintf(w, "%s Freed %s: %d file(s) and DataVolume(s) removed.\n", pal.Icon("✓"), bytesText(res.Bytes), len(res.Removed))
	case !shown && !res.Plan.Empty():
		renderPrunePlan(w, &res.Plan, pal)
		if isoDryRun {
			fmt.Fprintln(w, "\nDry run: nothing was changed.")
		}
	case res.Plan.Empty():
		fmt.Fprintln(w, "Nothing to prune: every ISO, installer and DataVolume is in use.")
	}
	return err
}

func runIsoDelete(cmd *cobra.Command, args []string) error {
	env, err := isosEnv(cmd)
	if err != nil {
		return err
	}
	ew := cmd.ErrOrStderr()
	shown := false
	res, err := isos.Delete(cmd.Context(), env, isos.DeleteOptions{
		Namespace: namespace, Args: args, DryRun: idlDryRun, Yes: idlYes,
		Confirm: confirmOnTerminal(os.Stdin, ew),
		ShowPlan: func(p *isos.Plan) {
			renderPrunePlan(ew, p, palette(ew))
			fmt.Fprintln(ew)
			shown = true
		},
	})
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if rerr := writeJSON(w, res); rerr != nil && err == nil {
			err = rerr
		}
		return err
	}
	switch {
	case len(res.Deleted) > 0:
		fmt.Fprintf(w, "%s Deleted %d thing(s), freeing %s.\n", pal.Icon("✓"), len(res.Deleted), bytesText(res.Bytes))
	case !shown && !res.Plan.Empty():
		renderPrunePlan(w, &res.Plan, pal)
		if idlDryRun {
			fmt.Fprintln(w, "\nDry run: nothing was changed.")
		}
	}
	return err
}

func renderPrunePlan(w io.Writer, p *isos.Plan, pal ui.Palette) {
	for _, v := range p.VSNOs {
		fmt.Fprintf(w, "  %s vSNO %s's ISO and build directory\n", pal.Red("remove"), v.Name)
	}
	for _, e := range p.Installers {
		fmt.Fprintf(w, "  %s %s  %s\n", pal.Red("remove"), e.Name, pal.Dim(bytesText(e.Size)+" "+e.Version))
	}
	for _, d := range p.Disks {
		fmt.Fprintf(w, "  %s DataVolume %s  %s\n", pal.Red("remove"), d.Name, pal.Dim(bytesText(d.Size)))
	}
	fmt.Fprintf(w, "\n%s\n", pal.Dim("Frees "+bytesText(p.Bytes)+" in the builder and on LVMS."))
}

func runIsoEject(cmd *cobra.Command, args []string) error {
	env, err := isosEnv(cmd)
	if err != nil {
		return err
	}
	changed, err := guest.EjectISO(cmd.Context(), env.Env, namespace, args[0])
	if err != nil {
		return err
	}
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if !changed {
		fmt.Fprintf(w, "vSNO %s has no install ISO to eject.\n", args[0])
		return nil
	}
	fmt.Fprintf(w, "%s Ejected vSNO %s's install ISO; it boots from its root disk now.\n", pal.Icon("✓"), args[0])
	return nil
}
