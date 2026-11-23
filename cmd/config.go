package cmd

import (
	"fmt"
	"os"

	"github.com/sebrandon1/vsno/lib/config"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show and change the settings vsno remembers between runs",
	Long: `config reads and writes the settings that do not change between runs, so
they need not be retyped as flags.

They live in $XDG_CONFIG_HOME/vsno/config (~/.config/vsno/config), one
KEY=VALUE per line. Nothing secret is stored there: a setting names the path to
a credentials file, never its contents.

A flag beats the environment, which beats the file:

  seed.sync.to        --to         VSNO_SEED_SYNC_TO
  seed.sync.authfile  --authfile   VSNO_SEED_SYNC_AUTHFILE
  seed.sync.ca        --ca         VSNO_SEED_SYNC_CA`,
	Args: cobra.NoArgs,
	RunE: runConfigList,
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the settings file's path",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		p, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), p)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Print one setting",
	Args:  oneArg("name a setting, as `vsno config` lists them"),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := config.Key(args[0])
		if err := config.Check(key); err != nil {
			return err
		}
		v, ok, err := config.Get(key)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s is not set; `vsno config set %s <value>` sets it", key, key)
		}
		fmt.Fprintln(cmd.OutOrStdout(), v)
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set one setting",
	Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 2 {
			return fmt.Errorf("name a setting and its value, as `vsno config set %s <value>`",
				string(config.SeedSyncTo))
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		key := config.Key(args[0])
		if err := config.Set(key, args[1]); err != nil {
			return err
		}
		p, _ := config.Path()
		fmt.Fprintf(cmd.OutOrStdout(), "Set %s in %s.\n", key, p)
		return nil
	},
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Forget one setting",
	Args:  oneArg("name a setting, as `vsno config` lists them"),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := config.Key(args[0])
		if err := config.Unset(key); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Unset %s.\n", key)
		return nil
	},
}

func init() {
	configCmd.AddCommand(configPathCmd, configGetCmd, configSetCmd, configUnsetCmd)
	rootCmd.AddCommand(configCmd)
}

// configRow is one line of `vsno config`: the setting, where its value came
// from, and the value.
type configRow struct {
	Key   string `json:"key"`
	What  string `json:"what"`
	From  string `json:"from"` // file, $VAR, or unset
	Value string `json:"value,omitempty"`
}

func runConfigList(cmd *cobra.Command, _ []string) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	file, err := config.Load()
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	rows := make([]configRow, 0, len(config.Settings))
	for _, s := range config.Settings {
		row := configRow{Key: string(s.Key), What: s.What, From: "unset"}
		switch {
		case file[s.Key] != "":
			row.From, row.Value = "file", file[s.Key]
		case os.Getenv(s.Env) != "":
			row.From, row.Value = "$"+s.Env, os.Getenv(s.Env)
		}
		rows = append(rows, row)
	}
	if outputFormat == formatJSON {
		return writeJSON(w, rows)
	}
	pal := palette(w)
	fmt.Fprintf(w, "%s\n", pal.Dim(path))
	var t ui.Table
	t.Row(pal.Bold("KEY"), pal.Bold("VALUE"), pal.Bold("FROM"), pal.Bold("WHAT"))
	for _, r := range rows {
		t.Row(r.Key, orDash(r.Value), r.From, pal.Dim(r.What))
	}
	t.Write(w)
	return nil
}

// configFor reads the settings file once, for a command that may take its
// defaults from it.
func configFor() map[config.Key]string {
	file, err := config.Load()
	if err != nil {
		return nil // an unreadable file changes nothing, as documented
	}
	return file
}
