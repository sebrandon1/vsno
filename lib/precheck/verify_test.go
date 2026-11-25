package precheck

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// oneNodeProber answers a Probe for one node with a fixed result.
type oneNodeProber struct {
	hw   NodeHardware
	errs map[string]error
}

func (p oneNodeProber) Probe(_ context.Context, nodes []string, _ func(string, error)) (map[string]NodeHardware, map[string]error) {
	hw := map[string]NodeHardware{}
	errs := map[string]error{}
	for _, n := range nodes {
		if err := p.errs[n]; err != nil {
			errs[n] = err
			continue
		}
		hw[n] = p.hw
	}
	return hw, errs
}

func TestVerifyDiskMatchesNames(t *testing.T) {
	hw := NodeHardware{Disks: []Disk{{
		Name: "sdb", SizeBytes: 1920 * 1_000_000_000,
		ByID: []string{"wwn-0x5000000000000002"},
	}}}
	p := oneNodeProber{hw: hw}
	for _, name := range []string{
		"/dev/disk/by-id/wwn-0x5000000000000002",
		"wwn-0x5000000000000002",
		"/dev/sdb",
		"sdb",
	} {
		d, err := VerifyDisk(context.Background(), p, "worker-1", name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if d.Name != "sdb" {
			t.Errorf("%s matched %q", name, d.Name)
		}
	}
}

func TestVerifyDiskNotFoundAndErrors(t *testing.T) {
	p := oneNodeProber{hw: NodeHardware{Disks: []Disk{{Name: "sdb"}}}}
	if _, err := VerifyDisk(context.Background(), p, "worker-1", "/dev/sdz"); err == nil || !strings.Contains(err.Error(), "saw: sdb") {
		t.Errorf("not found: %v", err)
	}
	failing := oneNodeProber{errs: map[string]error{"worker-1": errors.New("pod failed")}}
	if _, err := VerifyDisk(context.Background(), failing, "worker-1", "/dev/sdb"); err == nil || !strings.Contains(err.Error(), "pod failed") {
		t.Errorf("probe failure: %v", err)
	}
	degraded := oneNodeProber{hw: NodeHardware{Errors: []string{"lsblk failed"}}}
	if _, err := VerifyDisk(context.Background(), degraded, "worker-1", "/dev/sdb"); err == nil || !strings.Contains(err.Error(), "lsblk failed") {
		t.Errorf("degraded probe: %v", err)
	}
}
