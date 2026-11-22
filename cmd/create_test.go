package cmd

import (
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/snapshot"
)

func TestCheckCreateArgs(t *testing.T) {
	for _, c := range []struct {
		desc         string
		name         string
		fromSnapshot bool
		version      string
		want         string // substring of the error; "" means no error
	}{
		{"install", "a", false, "4.22", ""},
		{"restore", "a", true, "", ""},
		{"no version", "a", false, "", "--version is required"},
		{"no name to restore", "", true, "", "--from-snapshot restores a vSNO under its own name"},
	} {
		err := checkCreateArgs(c.name, c.fromSnapshot, c.version)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v, want nil", c.desc, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: nil, want an error containing %q", c.desc, c.want)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: %v, want %q", c.desc, err, c.want)
		}
	}
}

func TestRestoreTarget(t *testing.T) {
	if got, err := restoreTarget(&snapshot.Entry{Name: "a"}, "a"); err != nil || got != "a" {
		t.Errorf("restoreTarget = %q, %v; want a, nil", got, err)
	}

	_, err := restoreTarget(nil, "a")
	if err == nil {
		t.Fatal("a missing snapshot must be an error")
	}
	for _, want := range []string{"no snapshot named a", "vsno snapshot create a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
}
