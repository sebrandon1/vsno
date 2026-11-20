package builder

import (
	"archive/tar"
	"bytes"
	"slices"
)

// Tar packs files (path → content) for a script's stdin, sorted by path.
func Tar(files map[string]string) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o600, Size: int64(len(files[p]))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(files[p])); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
