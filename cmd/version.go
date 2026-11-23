package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the vsno version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if outputFormat == formatJSON {
			return writeJSON(cmd.OutOrStdout(), map[string]string{"version": cliVersion})
		}
		fmt.Fprintln(cmd.OutOrStdout(), cliVersion)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
