package setup

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sebrandon1/vsno/lib/precheck"
)

// capacityChecks only warn: `vsno create` enforces capacity per VM later, and
// an existing environment may hold the memory now.
var capacityChecks = map[string]bool{"cpu": true, "memory": true}

// resolve merges the user's settings over the precheck's suggestion and
// decides which precheck failures stop setup.
func resolve(rep *precheck.Report, user precheck.Suggested) (precheck.Suggested, []string, error) {
	s := merge(user, rep.Suggested)
	var hard, warnings []string
	for _, r := range rep.Results {
		if r.Status != precheck.Fail {
			continue
		}
		switch {
		case r.Node != "" && r.Node != s.Node:
			// Another node; not used.
		case r.Node != "" && capacityChecks[r.Check]:
			warnings = append(warnings, r.Name()+": "+r.Detail)
		case r.Check == "disk" && s.Disk != "":
			// The user named a disk: the storage step re-probes it right before
			// erasing it, which is the authoritative check.
			warnings = append(warnings, r.Name()+": "+r.Detail)
		case r.Check == "Eligible node" && s.Node != "":
			// The user picked a node.
		default:
			hard = append(hard, r.Name()+": "+r.Detail)
		}
	}
	if len(hard) > 0 {
		return s, warnings, fmt.Errorf("precheck failed: %s", strings.Join(hard, "; "))
	}
	if s.Node == "" {
		return s, warnings, errors.New("no node selected: pass --node, or run with the hardware probe so precheck can suggest one")
	}
	return s, warnings, nil
}

// merge returns user with empty fields filled from suggested. A user-chosen
// node different from the suggested one drops the suggested disk, which belongs
// to the other node.
func merge(user, suggested precheck.Suggested) precheck.Suggested {
	s := user
	if s.Node == "" {
		s.Node = suggested.Node
	}
	if s.Disk == "" && s.Node == suggested.Node {
		s.Disk, s.DiskSizeGB = suggested.Disk, suggested.DiskSizeGB
	}
	fill := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	fill(&s.HostVersion, suggested.HostVersion)
	fill(&s.HostDNS, suggested.HostDNS)
	fill(&s.LVMSChannel, suggested.LVMSChannel)
	fill(&s.CNVChannel, suggested.CNVChannel)
	return s
}
