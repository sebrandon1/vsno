// Package config is vsno's settings file: the handful of values that do not
// change between runs, kept in $XDG_CONFIG_HOME/vsno/config so they need not
// be retyped as flags. Flags still win; the file is only a default.
//
// The file is KEY=VALUE, one per line, and # starts a comment. Nothing
// secret is ever stored in it: a setting names the path to a credentials
// file, never the file's contents.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sebrandon1/go-toolbox/confkit"
)

// Key is a setting's name in the file.
type Key string

// The settings vsno knows about. A key in the file that is not one of these
// is reported rather than silently ignored: a typo would otherwise look like
// a setting that does nothing.
const (
	// SeedSyncTo is where `vsno seed sync` pushes a seed by default.
	SeedSyncTo Key = "seed.sync.to"
	// SeedSyncAuthFile is the credentials file for that registry.
	SeedSyncAuthFile Key = "seed.sync.authfile"
	// SeedSyncCA is that registry's CA certificate, when it is not publicly
	// trusted.
	SeedSyncCA Key = "seed.sync.ca"
)

// Setting is one key, with what it means, how the environment can override it
// and which flag beats both.
type Setting struct {
	Key   Key    `json:"key"`
	What  string `json:"what"`
	Env   string `json:"env"`
	Flag  string `json:"flag"`
	Value string `json:"value,omitempty"` // as the file has it
}

// Settings are the known settings, in the order they are listed.
var Settings = []Setting{
	{Key: SeedSyncTo, What: "where `vsno seed sync` pushes a seed", Env: "VSNO_SEED_SYNC_TO", Flag: "--to"},
	{Key: SeedSyncAuthFile, What: "credentials for that registry", Env: "VSNO_SEED_SYNC_AUTHFILE", Flag: "--authfile"},
	{Key: SeedSyncCA, What: "that registry's CA certificate", Env: "VSNO_SEED_SYNC_CA", Flag: "--ca"},
}

// Path is the settings file: $XDG_CONFIG_HOME/vsno/config, or
// ~/.config/vsno/config.
func Path() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding your home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "vsno", "config"), nil
}

// Load reads the file. A missing file is an empty map, not an error.
func Load() (map[Key]string, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	kv, err := confkit.LoadKV(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[Key]string{}, nil // nothing configured yet
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	out := make(map[Key]string, len(kv))
	for k, v := range kv {
		out[Key(k)] = v
	}
	return out, nil
}

// List is the settings with the values the file gives them.
func List() ([]Setting, error) {
	file, err := Load()
	if err != nil {
		return nil, err
	}
	out := make([]Setting, len(Settings))
	copy(out, Settings)
	for i := range out {
		out[i].Value = file[out[i].Key]
	}
	return out, nil
}

// Get reads one setting from the file.
func Get(k Key) (string, bool, error) {
	file, err := Load()
	if err != nil {
		return "", false, err
	}
	v, ok := file[k]
	return v, ok && v != "", nil
}

// Set writes one setting, leaving the file's comments and order as they are.
func Set(k Key, v string) error {
	if err := Check(k); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := confkit.UpdateKV(path, map[string]string{string(k): v}); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// Unset removes one setting, and its line: unsetting what is not set is not an
// error. The file's other lines and comments are kept.
func Unset(k Key) error {
	if err := Check(k); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- vsno's own file
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if key, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(key) == string(k) {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n") + "\n"
	if err := confkit.AtomicWriteFile(path, []byte(out), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// Check reports whether k is a setting vsno has, naming the ones it does when
// it is not.
func Check(k Key) error {
	for _, s := range Settings {
		if s.Key == k {
			return nil
		}
	}
	return fmt.Errorf("unknown setting %q; the ones vsno has are %s", k, strings.Join(names(), ", "))
}

// names lists the known keys, sorted for a stable message.
func names() []string {
	out := make([]string, 0, len(Settings))
	for _, s := range Settings {
		out = append(out, string(s.Key))
	}
	sort.Strings(out)
	return out
}

// Resolve picks a setting's value the way vsno reads it everywhere: the flag if
// it was given, then the environment, then the file, then "". The environment
// variable is the setting's Env, and expand turns a path in the file into an
// absolute one ("" leaves it as written).
func Resolve(s Setting, flagValue string, fromFile map[Key]string, expand bool) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(s.Env); v != "" {
		return v
	}
	v := fromFile[s.Key]
	if v == "" {
		return ""
	}
	if expand {
		return confkit.ExpandHome(v)
	}
	return v
}

// SettingFor returns the setting with key k.
func SettingFor(k Key) (Setting, bool) {
	for _, s := range Settings {
		if s.Key == k {
			return s, true
		}
	}
	return Setting{}, false
}
