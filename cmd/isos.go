package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/isos"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	isKeep   int
	isYes    bool
	isDryRun bool
)

var isosCmd = &cobra.Command{
	Use:   "isos",
	Short: "Show, and clean up, what vSNO creates leave in the builder and on LVMS",
	Long: `isos lists what the environment holds beyond its vSNOs: the ISOs the builder
serves, the installer's build directories, the cached openshift-install
binaries (one per release) and the ISO DataVolumes, with what still uses each.
It ends with the builder volume's usage.`,
	RunE: runIsos,
}

var isosPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove the ISOs, installers and ISO DataVolumes no vSNO uses",
	Long: `prune removes what a finished or failed create left behind: the served ISO and
build directory of vSNOs that no longer exist, the openshift-install binaries
no vSNO runs (--keep N keeps the N newest), and their ISO DataVolumes. It
lists them and asks you to type "prune" first, unless --yes. --dry-run only
lists.`,
	Args: cobra.NoArgs,
	RunE: runIsosPrune,
}

var isosEjectCmd = &cobra.Command{
	Use:   "eject <vsno>",
	Short: "Remove a vSNO's install ISO, which it no longer boots from",
	Long: `eject removes the install ISO from a vSNO that has finished installing: the
CD-ROM and its volume from the VM, the ISO DataVolume, and the builder's copy.
create does this itself when the install finishes; eject is for vSNOs it
didn't, such as ones from an older vsno.`,
	Args: cobra.ExactArgs(1),
	RunE: runIsosEject,
}

func init() {
	isosPruneCmd.Flags().IntVar(&isKeep, "keep", 0, "keep the N newest unused installers")
	isosPruneCmd.Flags().BoolVar(&isYes, "yes", false, "don't ask for confirmation")
	isosPruneCmd.Flags().BoolVar(&isDryRun, "dry-run", false, "list what would be removed and change nothing")
	isosCmd.AddCommand(isosPruneCmd, isosEjectCmd)
	rootCmd.AddCommand(isosCmd)
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

func runIsos(cmd *cobra.Command, _ []string) error {
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

func renderIsos(w io.Writer, rep *isos.Report, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return writeJSON(w, rep)
	}
	if len(rep.Entries) == 0 && len(rep.Disks) == 0 {
		fmt.Fprintf(w, "Nothing in %s: no ISOs, installer caches or ISO DataVolumes.\n", namespace)
		return nil
	}
	var t ui.Table
	t.Row(pal.Bold("WHAT"), pal.Bold("NAME"), pal.Bold("VERSION"), pal.Bold("SIZE"), pal.Bold("USE"))
	for _, e := range rep.Entries {
		t.Row(string(e.Kind), e.Name, dash(e.Version), bytesText(e.Size), useText(e.Use, e.Users))
	}
	for _, d := range rep.Disks {
		t.Row("data volume", d.Name, "-", bytesText(d.Size), useText(d.Use, d.Users))
	}
	t.Write(w)
	if rep.Total > 0 {
		pct := float64(rep.Used) / float64(rep.Total) * 100
		line := fmt.Sprintf("\nBuilder volume: %s of %s used (%.0f%%), %s free", bytesText(rep.Used), bytesText(rep.Total), pct, bytesText(rep.Total-rep.Used))
		if pct >= 80 {
			line = pal.Yellow(line)
		}
		fmt.Fprintln(w, line)
	}
	if rep.DiskBytes > 0 {
		fmt.Fprintf(w, "ISO DataVolumes on LVMS: %s\n", bytesText(rep.DiskBytes))
	}
	return nil
}

// dash is s, or "-" when empty.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

func runIsosPrune(cmd *cobra.Command, _ []string) error {
	env, err := isosEnv(cmd)
	if err != nil {
		return err
	}
	ew := cmd.ErrOrStderr()
	shown := false
	res, err := isos.Prune(cmd.Context(), env, isos.PruneOptions{
		Namespace: namespace, DryRun: isDryRun, Yes: isYes, Keep: isKeep,
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
	case len(res.Removed) > 0:
		fmt.Fprintf(w, "%s Freed %s: %d file(s) and DataVolume(s) removed.\n", pal.Icon("✓"), bytesText(res.Bytes), len(res.Removed))
	case !shown && !res.Plan.Empty():
		renderPrunePlan(w, &res.Plan, pal)
		if isDryRun {
			fmt.Fprintln(w, "\nDry run: nothing was changed.")
		}
	case res.Plan.Empty():
		fmt.Fprintln(w, "Nothing to prune: every ISO, installer and DataVolume is in use.")
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

func runIsosEject(cmd *cobra.Command, args []string) error {
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
