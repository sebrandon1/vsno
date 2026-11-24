package doctor

import (
	"context"
	"fmt"

	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/teardown"
)

// volumeLeaks finds PersistentVolumes the environment's claims left behind: the
// namespace is gone, the volume is Released, and its disk is still allocated.
func volumeLeaks(ctx context.Context, e Env, ns string) ([]Finding, error) {
	pvs, err := teardown.Allocated(ctx, e.Kube, ns)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, pv := range pvs {
		// Before a teardown every volume is Bound and in use: only a Released
		// one has lost its claim and is holding a disk for nothing.
		if pv.Phase != "Released" {
			continue
		}
		out = append(out, Finding{
			Result: precheck.Result{Group: "doctor", Check: "storage",
				Status: precheck.Warn,
				Detail: fmt.Sprintf("PV %s (%s, %s, was %s) is still allocated", pv.Name, pv.Phase, pv.Size.String(), pv.Claim),
				Hint:   "its LogicalVolume is stuck: `vsno teardown` reports it too, and the hint there finds the holder"},
		})
	}
	return out, nil
}

// registryRoom reports a registry the next seed will not fit in, using the same
// floors `seed precheck` applies.
func registryRoom(ctx context.Context, e Env, ns string) []Finding {
	if e.Volume == nil {
		return nil
	}
	used, total, err := e.Volume(ctx, ns)
	if err != nil {
		return []Finding{unreadable("the registry's volume", err)}
	}
	free := total - used
	if free >= seed.RoomWarn() {
		return nil
	}
	status := precheck.Warn
	if free < seed.RoomFail() {
		status = precheck.Fail
	}
	return []Finding{{
		Result: precheck.Result{Group: "doctor", Check: "registry space", Status: status,
			Detail: fmt.Sprintf("%sGi free of %sGi; a seed image needs about %sGi", gib(free), gib(total), gib(seed.RoomFail())),
			Hint:   "`vsno cache prune` or `vsno cache gc` frees space"},
	}}
}

// gib renders a byte count as the tables do.
func gib(n int64) string {
	if n < 0 {
		n = 0
	}
	return fmt.Sprintf("%.1f", float64(n)/(1<<30))
}
