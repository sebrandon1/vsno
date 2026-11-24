package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/config"
)

// With XDG_CONFIG_HOME isolated, set → get → path → unset behaves, and the
// seed sync defaults follow the file unless a flag says otherwise.
func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, s := range config.Settings {
		t.Setenv(s.Env, "")
	}
	t.Cleanup(func() {
		ssTo, ssFrom, ssAuth, ssCA = "", "", "", ""
		outputFormat = formatTable
	})

	var b bytes.Buffer
	configCmd.SetOut(&b)
	t.Cleanup(func() { configCmd.SetOut(nil) })
	if err := configSetCmd.RunE(configSetCmd, []string{"seed.sync.to", "quay.io/me/seeds"}); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if err := configGetCmd.RunE(configGetCmd, []string{"seed.sync.to"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "quay.io/me/seeds") {
		t.Errorf("get: %q", b.String())
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file was not written: %v", err)
	}

	// A configured destination applies only when neither flag is given.
	if to, _, _ := syncDefaults(); to != "quay.io/me/seeds" {
		t.Errorf("default to = %q", to)
	}
	ssFrom = "quay.io/x/y:4.22.0"
	if to, _, _ := syncDefaults(); to != "" {
		t.Errorf("a pull inherited a destination: %q", to)
	}
	ssFrom = ""
	ssTo = "--to was given"
	if to, _, _ := syncDefaults(); to != "--to was given" {
		t.Errorf("flag did not win: %q", to)
	}

	b.Reset()
	if err := configUnsetCmd.RunE(configUnsetCmd, []string{"seed.sync.to"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := config.Get(config.SeedSyncTo); ok {
		t.Error("still set after unset")
	}
}

func TestConfigList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, s := range config.Settings {
		t.Setenv(s.Env, "")
	}
	t.Cleanup(func() { outputFormat = formatTable })
	if err := config.Set(config.SeedSyncTo, "quay.io/me/seeds"); err != nil {
		t.Fatal(err)
	}
	caSet, _ := config.SettingFor(config.SeedSyncCA)
	t.Setenv(caSet.Env, "/etc/ca.crt")

	var b bytes.Buffer
	configCmd.SetOut(&b)
	t.Cleanup(func() { configCmd.SetOut(nil) })
	if err := runConfigList(configCmd, nil); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"KEY", "seed.sync.to", "quay.io/me/seeds", "file",
		"$" + caSet.Env, "/etc/ca.crt", "unset"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestOneArgNamesWhatIsWanted(t *testing.T) {
	for _, c := range []struct {
		args []string
		err  string
	}{
		{nil, "name the seed as"},
		{[]string{"a", "b"}, "name the seed as"},
	} {
		if err := oneArg("the seed as <vsno>[:<version>]")(nil, c.args); err == nil || !strings.Contains(err.Error(), "name the seed as <vsno>") {
			t.Errorf("oneArg(%v) = %v", c.args, err)
		}
	}
	if err := oneArg("x")(nil, []string{"a"}); err != nil {
		t.Errorf("oneArg with one argument: %v", err)
	}
	if err := someArgs("x")(nil, nil); err == nil {
		t.Error("someArgs with none: no error")
	}
	if err := someArgs("x")(nil, []string{"a", "b"}); err != nil {
		t.Errorf("someArgs: %v", err)
	}
}

// Each config subcommand that prints something has a -o json shape.
func TestConfigJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, s := range config.Settings {
		t.Setenv(s.Env, "")
	}
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = formatTable })

	var b bytes.Buffer
	configCmd.SetOut(&b)
	t.Cleanup(func() { configCmd.SetOut(nil) })

	decode := func(v any) {
		t.Helper()
		if err := json.Unmarshal(b.Bytes(), v); err != nil {
			t.Fatalf("%s: %v", b.String(), err)
		}
		b.Reset()
	}

	if err := configSetCmd.RunE(configSetCmd, []string{"seed.sync.to", "quay.io/me/seeds"}); err != nil {
		t.Fatal(err)
	}
	var set configSetResult
	decode(&set)
	if set.Key != "seed.sync.to" || set.Value != "quay.io/me/seeds" || set.Path == "" {
		t.Errorf("set json = %+v", set)
	}

	if err := configGetCmd.RunE(configGetCmd, []string{"seed.sync.to"}); err != nil {
		t.Fatal(err)
	}
	var got configValueResult
	decode(&got)
	if got.Key != "seed.sync.to" || got.Value != "quay.io/me/seeds" {
		t.Errorf("get json = %+v", got)
	}

	if err := configPathCmd.RunE(configPathCmd, nil); err != nil {
		t.Fatal(err)
	}
	var path configPathResult
	decode(&path)
	if path.Path != set.Path {
		t.Errorf("path json = %+v, want %q", path, set.Path)
	}

	if err := configUnsetCmd.RunE(configUnsetCmd, []string{"seed.sync.to"}); err != nil {
		t.Fatal(err)
	}
	var unset configUnsetResult
	decode(&unset)
	if unset.Key != "seed.sync.to" {
		t.Errorf("unset json = %+v", unset)
	}
}
