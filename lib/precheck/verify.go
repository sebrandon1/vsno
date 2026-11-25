package precheck

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// VerifyDisk re-probes node and returns the whole disk that disk names, so a
// destructive step can check a disk again right before it uses it.
func VerifyDisk(ctx context.Context, p Prober, node, disk string) (Disk, error) {
	hw, errs := p.Probe(ctx, []string{node}, nil)
	if err := errs[node]; err != nil {
		return Disk{}, fmt.Errorf("re-probing %s: %w", node, err)
	}
	h, ok := hw[node]
	if !ok {
		return Disk{}, fmt.Errorf("no hardware reported for %s", node)
	}
	if len(h.Errors) > 0 {
		return Disk{}, fmt.Errorf("probing %s: %s", node, strings.Join(h.Errors, "; "))
	}
	want := deviceKey(disk)
	var names []string
	for _, d := range h.Disks {
		names = append(names, d.Name)
		if d.Name == want || filepath.Base(d.PreferredByID()) == want || slices.Contains(d.ByID, want) {
			return d, nil
		}
	}
	return Disk{}, fmt.Errorf("disk %s not found on %s (saw: %s)", disk, node, strings.Join(names, ", "))
}

// deviceKey reduces /dev/disk/by-id/wwn-… or /dev/sdb to the name the probe uses.
func deviceKey(disk string) string {
	disk = strings.TrimPrefix(disk, "/dev/disk/by-id/")
	return strings.TrimPrefix(disk, "/dev/")
}
