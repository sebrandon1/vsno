package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/report"
	"github.com/spf13/cobra"
)

var rpMarkdown bool

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Write everything a bug report or a demo needs, in one document",
	Long: `report reads the environment once and writes it as one JSON document: the
status, the vSNOs, the release cache, the seed images and the builder's
leftovers, plus a warnings list for whatever could not be read.

It is JSON unless --markdown asks for prose and tables. Nothing it writes
carries credentials: registry logins, S3 keys and the pull secret are named,
never quoted.

  vsno report > vsno-report.json
  vsno report --markdown > vsno-report.md`,
	Args: cobra.NoArgs,
	RunE: runReport,
}

func init() {
	reportCmd.Flags().BoolVar(&rpMarkdown, "markdown", false, "write Markdown tables instead of JSON")
	rootCmd.AddCommand(reportCmd)
}

func runReport(cmd *cobra.Command, _ []string) error {
	// report has no table view; the global -o's default is table, so only an
	// explicit -o table is a mistake worth naming.
	if fl := cmd.Flags().Lookup("output"); fl != nil && fl.Changed && outputFormat != formatJSON {
		return fmt.Errorf("report writes JSON; use --markdown for Markdown, or -o json")
	}
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	if err := useEnvironment(cmd.Context(), c.Kube); err != nil {
		return err
	}
	env, err := guestEnv(cmd.Context(), c, 0)
	if err != nil {
		return err
	}
	rep, err := report.Collect(cmd.Context(), report.Env{Env: env, Config: c.Config}, namespace)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if rpMarkdown {
		return renderReportMarkdown(w, rep, time.Now())
	}
	return writeJSON(w, rep)
}

// renderReportMarkdown writes the report as headings and tables, for pasting
// into an issue.
func renderReportMarkdown(w io.Writer, rep *report.Report, now time.Time) error {
	fmt.Fprintf(w, "# vsno report: %s\n\n", rep.Namespace)
	if rep.Status != nil {
		fmt.Fprintf(w, "- healthy: %v\n", rep.Status.Healthy())
		if rep.Status.Next != "" {
			fmt.Fprintf(w, "- next: %s\n", rep.Status.Next)
		}
	}
	if len(rep.Warnings) > 0 {
		fmt.Fprint(w, "\n## Warnings\n\n")
		for _, warn := range rep.Warnings {
			fmt.Fprintf(w, "- %s\n", warn)
		}
	}
	if len(rep.VSNOs) > 0 {
		fmt.Fprint(w, "\n## vSNOs\n\n| NAME | STATE | VERSION | AGE |\n|---|---|---|---|\n")
		for _, g := range rep.VSNOs {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", g.Name, g.State, orDash(g.ShownVersion()), ageSince(now, g.Created))
		}
	}
	if rep.Cache != nil && len(rep.Cache.Entries) > 0 {
		fmt.Fprint(w, "\n## Release cache\n\n| VERSION | STATE | SIZE |\n|---|---|---|\n")
		for _, e := range rep.Cache.Entries {
			fmt.Fprintf(w, "| %s | %s | %s |\n", orDash(e.Version), e.State, bytesText(e.SizeBytes))
		}
		fmt.Fprintf(w, "\nregistry volume: %s of %s used\n", bytesText(rep.Cache.Used), bytesText(rep.Cache.Total))
	}
	if len(rep.Seeds) > 0 {
		fmt.Fprint(w, "\n## Seed images\n\n| IMAGE | VERSION | SIZE |\n|---|---|---|\n")
		for _, s := range rep.Seeds {
			fmt.Fprintf(w, "| %s | %s | %s |\n", s.Ref, orDash(s.Version), bytesText(s.SizeBytes))
		}
	}
	return nil
}
