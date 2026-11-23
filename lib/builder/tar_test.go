package builder

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
)

// files reads a tar back as name → content, with the mode it was written with.
func files(t *testing.T, b []byte) []tar.Header {
	t.Helper()
	var out []tar.Header
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading the tar: %v", err)
		}
		out = append(out, *h)
	}
}

func TestTar(t *testing.T) {
	b, err := Tar(map[string]string{"b.json": "second", "a.crt": "first"})
	if err != nil {
		t.Fatal(err)
	}
	got := files(t, b)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// Sorted, so a script can rely on a stable layout, and readable only by
	// the user that wrote them: these carry registry credentials.
	if names := []string{got[0].Name, got[1].Name}; !slices.Equal(names, []string{"a.crt", "b.json"}) {
		t.Errorf("entries = %q, want them sorted", names)
	}
	for _, h := range got {
		if h.Mode != 0o600 {
			t.Errorf("%s mode = %o, want 600", h.Name, h.Mode)
		}
	}
	if got[0].Size != int64(len("first")) || got[1].Size != int64(len("second")) {
		t.Errorf("sizes = %d, %d", got[0].Size, got[1].Size)
	}
}

func TestTarEmpty(t *testing.T) {
	b, err := Tar(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := files(t, b); len(got) != 0 {
		t.Errorf("got %d entries, want none", len(got))
	}
}
