package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolate points XDG_CONFIG_HOME at a temp directory for one test.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, s := range Settings {
		t.Setenv(s.Env, "")
	}
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "vsno", "config"); p != want {
		t.Fatalf("Path() = %q, want %q", p, want)
	}
	return p
}

func TestMissingFileIsEmpty(t *testing.T) {
	isolate(t)
	file, err := Load()
	if err != nil || len(file) != 0 {
		t.Errorf("Load() = %v, %v; want an empty map and no error", file, err)
	}
	if _, ok, err := Get(SeedSyncTo); ok || err != nil {
		t.Errorf("Get = %v, %v; want unset and no error", ok, err)
	}
	if err := Unset(SeedSyncTo); err != nil {
		t.Errorf("Unset on a missing file: %v", err)
	}
}

func TestSetGetUnset(t *testing.T) {
	path := isolate(t)
	if err := Set(SeedSyncTo, "quay.io/me/seeds"); err != nil {
		t.Fatal(err)
	}
	if err := Set(SeedSyncAuthFile, "~/.config/containers/auth.json"); err != nil {
		t.Fatal(err)
	}
	prependComment(t, path)
	if v, ok, err := Get(SeedSyncTo); err != nil || !ok || v != "quay.io/me/seeds" {
		t.Errorf("Get = %q, %v, %v", v, ok, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := Set(SeedSyncTo, "quay.io/me/other"); err != nil { // overwrites in place
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# my seeds\n") || strings.Count(string(b), "seed.sync.to=") != 1 {
		t.Errorf("file after overwrite:\n%s", b)
	}
}

// Unset drops the setting's line and nothing else.
func TestUnsetKeepsTheRest(t *testing.T) {
	path := isolate(t)
	for k, v := range map[Key]string{SeedSyncTo: "quay.io/me/seeds", SeedSyncAuthFile: "~/auth.json"} {
		if err := Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	prependComment(t, path)
	if err := Unset(SeedSyncTo); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Get(SeedSyncTo); ok {
		t.Error("still set after Unset")
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# my seeds\n") || strings.Contains(string(b), "seed.sync.to=") {
		t.Errorf("file after unset:\n%s", b)
	}
	if !strings.Contains(string(b), "seed.sync.authfile=") {
		t.Errorf("unset removed another setting:\n%s", b)
	}
}

// prependComment puts a comment the user wrote at the top of the file.
func prependComment(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("# my seeds\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownKey(t *testing.T) {
	isolate(t)
	for _, c := range []func() error{
		func() error { return Set("seed.sync.nope", "x") },
		func() error { return Unset("seed.sync.nope") },
		func() error { return Check("seed.sync.nope") },
	} {
		err := c()
		if err == nil || !strings.Contains(err.Error(), "seed.sync.to") {
			t.Errorf("err = %v, want it to list the known settings", err)
		}
	}
}

// The flag wins over the environment, which wins over the file.
func TestResolvePrecedence(t *testing.T) {
	isolate(t)
	s, _ := SettingFor(SeedSyncTo)
	file := map[Key]string{SeedSyncTo: "from-file"}
	if got := Resolve(s, "", file, false); got != "from-file" {
		t.Errorf("file: %q", got)
	}
	t.Setenv(s.Env, "from-env")
	if got := Resolve(s, "", file, false); got != "from-env" {
		t.Errorf("env: %q", got)
	}
	if got := Resolve(s, "from-flag", file, false); got != "from-flag" {
		t.Errorf("flag: %q", got)
	}
	t.Setenv(s.Env, "")
	if got := Resolve(s, "", map[Key]string{}, false); got != "" {
		t.Errorf("nothing: %q", got)
	}
	home, _ := os.UserHomeDir()
	if got, want := Resolve(s, "", map[Key]string{SeedSyncTo: "~/ca.crt"}, true), filepath.Join(home, "ca.crt"); got != want {
		t.Errorf("expanded: %q, want %q", got, want)
	}
}

func TestListAndNames(t *testing.T) {
	isolate(t)
	if err := Set(SeedSyncTo, "quay.io/me/seeds"); err != nil {
		t.Fatal(err)
	}
	rows, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(Settings) {
		t.Fatalf("%d rows, want %d", len(rows), len(Settings))
	}
	byKey := map[Key]Setting{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if byKey[SeedSyncTo].Value != "quay.io/me/seeds" || byKey[SeedSyncCA].Value != "" {
		t.Errorf("rows = %+v", rows)
	}
	if got := names(); !slices.IsSorted(got) {
		t.Errorf("names() = %v, want sorted", got)
	}
}

// Without XDG_CONFIG_HOME the file lives under the home directory.
func TestPathFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "vsno", "config"); p != want {
		t.Errorf("Path() = %q, want %q", p, want)
	}
}

func TestSettingFor(t *testing.T) {
	if s, ok := SettingFor(SeedSyncTo); !ok || s.Env != "VSNO_SEED_SYNC_TO" || s.Flag != "--to" {
		t.Errorf("SettingFor(seed.sync.to) = %+v, %v", s, ok)
	}
	if _, ok := SettingFor("nope"); ok {
		t.Error("SettingFor(nope) found something")
	}
}

// A value is one line: a line break would add a line to the file, which could
// name another setting.
func TestSetRefusesLineBreaks(t *testing.T) {
	path := isolate(t)
	for _, v := range []string{"a\nb", "a\rb", "a\nseed.sync.ca=/etc/evil"} {
		if err := Set(SeedSyncTo, v); err == nil || !strings.Contains(err.Error(), "line break") {
			t.Errorf("Set(%q) = %v, want a refusal", v, err)
		}
	}
	if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), "seed.sync.ca") {
		t.Errorf("a line break smuggled a setting in:\n%s", b)
	}
	if v, ok, _ := Get(SeedSyncTo); ok {
		t.Errorf("the refused value was written: %q", v)
	}
}

// The defaults a user retypes: the environment and the output format.
func TestDefaultsAreSettings(t *testing.T) {
	for _, c := range []struct {
		key       Key
		env, flag string
	}{
		{Namespace, "VSNO_NAMESPACE", "-n"},
		{Output, "VSNO_OUTPUT", "-o"},
	} {
		s, ok := SettingFor(c.key)
		if !ok || s.Env != c.env || s.Flag != c.flag {
			t.Errorf("%s = %+v, %v", c.key, s, ok)
		}
	}
	isolate(t)
	if err := Set(Namespace, "vsno-dev"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := Get(Namespace)
	if err != nil || !ok || v != "vsno-dev" {
		t.Errorf("Get = %q, %v, %v", v, ok, err)
	}
}
