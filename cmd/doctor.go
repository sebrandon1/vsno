package cmd

import (
	"github.com/sebrandon1/vsno/lib/doctor"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var drFix bool

var doctorCmd = &cobra.Command{
	Use:   "doctor [<vsno>...]",
	Short: "Look for the states that make an environment look broken",
	Long: `doctor checks for the states that read as a hang or a boot failure but are
something vsno knows about:

  - an ImageBasedUpgrade whose Idle condition is stuck at a reason the
    Lifecycle Agent's controller does not act on (#125)
  - a seed generation the Lifecycle Agent is running that no vsno process is
    following (#127)
  - a vSNO whose kubelet and crio a half-finished seed generation left
    disabled, so it never answers (#129)
  - a Released PersistentVolume still holding the environment's disk (#102)
  - a registry too full for the next seed image

It only reads unless --fix is given, and --fix runs only two reversible
actions: enabling crio and kubelet on such a vSNO, and the status patch that
unsticks LCA's Idle condition. It never deletes anything.`,
	Args: cobra.ArbitraryArgs, // names are optional: none means every vSNO and the environment
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().BoolVar(&drFix, "fix", false, "apply the two reversible fixes; everything else is reported")
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	if err := useEnvironment(cmd.Context(), c.Kube); err != nil {
		return err
	}
	env, err := seedEnv(cmd, 0)
	if err != nil {
		return err
	}
	env.Defaults(namespace) // --fix needs env.Access after Run returns
	findings, err := doctor.Run(cmd.Context(), doctor.Env{Env: env}, namespace, args)
	if err != nil {
		return err
	}
	if drFix {
		for i := range findings {
			if findings[i].Action == "" || findings[i].VSNo == "" {
				continue
			}
			if ferr := doctor.Fix(cmd.Context(), env.Access(findings[i].VSNo), findings[i]); ferr != nil {
				findings[i].Detail += "; the fix failed: " + ferr.Error()
			} else {
				findings[i].Detail += "; fixed"
			}
		}
	}
	return renderDoctor(cmd, findings)
}

// renderDoctor prints the findings, or nothing when there are none: a healthy
// environment should not print a wall of green.
func renderDoctor(cmd *cobra.Command, findings []doctor.Finding) error {
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		if findings == nil {
			findings = []doctor.Finding{}
		}
		return writeJSON(w, findings)
	}
	if len(findings) == 0 {
		return nil
	}
	var t ui.Table
	t.Row(pal.Bold("CHECK"), pal.Bold("STATUS"), pal.Bold("DETAIL"), pal.Bold("FIX"))
	for _, f := range findings {
		t.Row(f.Check, doctorStatus(pal, f.Status), f.Detail, pal.Dim(f.Fix))
	}
	t.Write(w)
	return nil
}

// doctorStatus colors a finding the way precheck colors its rows.
func doctorStatus(pal ui.Palette, s precheck.Status) string {
	switch s {
	case precheck.Fail:
		return pal.Red("fail")
	case precheck.Warn:
		return pal.Yellow("warn")
	}
	return pal.Green("pass")
}
