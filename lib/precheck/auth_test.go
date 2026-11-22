package precheck

import (
	"path/filepath"
	"slices"
	"testing"
)

// The order is the lookup order podman and docker use: an explicit override,
// then the runtime directory, then the user's own files.
func TestDefaultAuthFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("REGISTRY_AUTH_FILE", "/etc/vsno/auth.json")

	got := DefaultAuthFiles()
	want := []string{
		"/etc/vsno/auth.json",
		"/run/user/1000/containers/auth.json",
		filepath.Join(home, ".config", "containers", "auth.json"),
		filepath.Join(home, ".docker", "config.json"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("DefaultAuthFiles = %v\nwant %v", got, want)
	}
}

func TestDefaultAuthFilesWithoutOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("REGISTRY_AUTH_FILE", "")

	got := DefaultAuthFiles()
	want := []string{
		filepath.Join(home, ".config", "containers", "auth.json"),
		filepath.Join(home, ".docker", "config.json"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("DefaultAuthFiles = %v\nwant %v", got, want)
	}
}
