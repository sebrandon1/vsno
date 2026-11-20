// Package ibu runs Image Based Upgrades between vSNOs: it prepares a vSNO
// to be upgraded (LCA, OADP, the backup location and the seed's pull secret)
// and drives LCA's ImageBasedUpgrade through its stages. Seed images
// themselves come from package seed.
package ibu

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/seed"
)

// Env is seed's: the host cluster, the builder, and oc and ssh to each vSNO.
type Env = seed.Env

// Stage is a stage of LCA's ImageBasedUpgrade, its spec.stage.
type Stage string

// The stages, in the order an upgrade normally takes them. Rollback leaves
// Upgrade for the original stateroot; Idle after Upgrade finalizes it.
const (
	StageIdle     Stage = "Idle"
	StagePrep     Stage = "Prep"
	StageUpgrade  Stage = "Upgrade"
	StageRollback Stage = "Rollback"
)

// ibuName is the ImageBasedUpgrade LCA creates and watches; there is one.
const ibuName = "upgrade"

// stageConditions are the condition types LCA sets while it moves to a stage
// and once it gets there. Idle has no in-progress condition of its own.
var stageConditions = map[Stage]struct{ inProgress, completed string }{
	StageIdle:     {"", "Idle"},
	StagePrep:     {"PrepInProgress", "PrepCompleted"},
	StageUpgrade:  {"UpgradeInProgress", "UpgradeCompleted"},
	StageRollback: {"RollbackInProgress", "RollbackCompleted"},
}

// failedReasons are the condition reasons LCA uses for a stage that can't
// complete: it failed, timed out, was refused, or is blocked by another flow.
var failedReasons = []string{"Failed", "TimedOut", "InvalidTransition", "Blocked", "AbortFailed", "FinalizeFailed"}

// IBU is the part of LCA's ImageBasedUpgrade vsno reads.
type IBU struct {
	Spec struct {
		Stage        Stage `json:"stage"`
		SeedImageRef struct {
			Version string `json:"version"`
			Image   string `json:"image"`
		} `json:"seedImageRef"`
	} `json:"spec"`
	Status struct {
		Conditions      guestctl.Conditions `json:"conditions"`
		ValidNextStages []Stage             `json:"validNextStages"`
		History         []History           `json:"history"`
	} `json:"status"`
}

// History is one stage LCA went through, with the phases inside it.
type History struct {
	Stage          Stage     `json:"stage"`
	StartTime      time.Time `json:"startTime"`
	CompletionTime time.Time `json:"completionTime,omitzero"`
	Phases         []Phase   `json:"phases,omitempty"`
}

// Phase is one of the steps LCA takes within a stage.
type Phase struct {
	Phase          string    `json:"phase"`
	StartTime      time.Time `json:"startTime"`
	CompletionTime time.Time `json:"completionTime,omitzero"`
}

// Progress is how far LCA has got toward a stage.
type Progress int

const (
	// Moving means LCA is still working toward the stage.
	Moving Progress = iota
	// Arrived means the stage completed.
	Arrived
	// Stuck means the stage can't complete: LCA failed it, or refused it.
	Stuck
)

// Toward says how far u has got toward stage s, and LCA's message about it.
// On a failure LCA's completed condition says only "Prep failed"; the
// in-progress one says why, so its message is the one reported.
func (u IBU) Toward(s Stage) (Progress, string) {
	c := stageConditions[s]
	done := u.Status.Conditions.Find(c.completed)
	var cur *guestctl.Condition
	if c.inProgress != "" {
		cur = u.Status.Conditions.Find(c.inProgress)
	}
	failed := func(x *guestctl.Condition) bool { return x != nil && slices.Contains(failedReasons, x.Reason) }
	switch {
	case done != nil && done.Status == "True" && u.Spec.Stage == s:
		return Arrived, done.Message
	case failed(cur) && cur.Message != "":
		return Stuck, cur.Message
	case failed(done):
		return Stuck, done.Message
	case failed(cur):
		return Stuck, cur.Message
	case cur != nil:
		return Moving, cur.Message
	}
	return Moving, ""
}

// precacheRe reads LCA's precache progress, "total: 190 (pulled: 12, failed: 0)".
var precacheRe = regexp.MustCompile(`total: (\d+) \(pulled: (\d+), failed: (\d+)\)`)

// Precache reads how many of the seed's images Prep has pulled from a Prep
// message; ok is false when the message doesn't say.
func Precache(msg string) (pulled, failed, total int, ok bool) {
	m := precacheRe.FindStringSubmatch(msg)
	if m == nil {
		return 0, 0, 0, false
	}
	total, _ = strconv.Atoi(m[1])
	pulled, _ = strconv.Atoi(m[2])
	failed, _ = strconv.Atoi(m[3])
	return pulled, failed, total, true
}

// read reads the vSNO's ImageBasedUpgrade; found is false before LCA made it.
func read(ctx context.Context, a guestctl.Access) (IBU, bool, error) {
	var u IBU
	found, err := guestctl.Get(ctx, a, &u, "get", "imagebasedupgrades", ibuName, "--ignore-not-found", "--request-timeout=20s")
	return u, found, err
}

// Identity is what an IBU must keep: everything but the version.
type Identity struct {
	Version            string `json:"version"`
	ClusterID          string `json:"clusterID"`
	InfrastructureName string `json:"infrastructureName"`
	Node               string `json:"node"`
	APIURL             string `json:"apiURL"`
}

// identity reads the vSNO's identity.
func identity(ctx context.Context, a guestctl.Access) (Identity, error) {
	var cv struct {
		Spec struct {
			ClusterID string `json:"clusterID"`
		} `json:"spec"`
		Status struct {
			Desired struct {
				Version string `json:"version"`
			} `json:"desired"`
		} `json:"status"`
	}
	var infra struct {
		Status struct {
			InfrastructureName string `json:"infrastructureName"`
			APIServerURL       string `json:"apiServerURL"`
		} `json:"status"`
	}
	var nodes guestctl.ObjectList
	if _, err := guestctl.Get(ctx, a, &cv, "get", "clusterversion", "version"); err != nil {
		return Identity{}, err
	}
	if _, err := guestctl.Get(ctx, a, &infra, "get", "infrastructure", "cluster"); err != nil {
		return Identity{}, err
	}
	if _, err := guestctl.Get(ctx, a, &nodes, "get", "nodes"); err != nil {
		return Identity{}, err
	}
	id := Identity{Version: cv.Status.Desired.Version, ClusterID: cv.Spec.ClusterID,
		InfrastructureName: infra.Status.InfrastructureName, APIURL: infra.Status.APIServerURL}
	if len(nodes.Items) > 0 {
		id.Node = nodes.Items[0].Metadata.Name
	}
	return id, nil
}

// Changed lists the fields that differ between before and after, other than
// the version, which an upgrade is meant to change.
func (before Identity) Changed(after Identity) []string {
	var out []string
	for _, f := range []struct{ name, a, b string }{
		{"clusterID", before.ClusterID, after.ClusterID},
		{"infrastructureName", before.InfrastructureName, after.InfrastructureName},
		{"node", before.Node, after.Node},
		{"apiURL", before.APIURL, after.APIURL},
	} {
		if f.a != f.b {
			out = append(out, f.name+" "+f.a+" → "+f.b)
		}
	}
	return out
}
