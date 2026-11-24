// Package ibu runs Image Based Upgrades between vSNOs: it prepares a vSNO
// to be upgraded (LCA, OADP, the backup location and the seed's pull secret)
// and drives LCA's ImageBasedUpgrade through its stages. Seed images
// themselves come from package seed.
package ibu

import (
	"context"
	"fmt"
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

// idleHandled are the reasons LCA's controller acts on for the Idle condition:
// handleAbortOrFinalize switches on exactly these. A False Idle condition with
// any other reason matches no case, so nothing moves it and the controller has
// nothing left to reconcile.
var idleHandled = []string{"Aborting", "AbortFailed", "Finalizing", "FinalizeFailed"}

// IdleHandled reports whether LCA's controller acts on this Idle condition
// reason.
func IdleHandled(reason string) bool { return slices.Contains(idleHandled, reason) }

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

// StuckIdle reports the Idle condition when LCA will not act on it: the spec
// is at Idle, the condition is False, and its reason is one LCA's controller
// has no case for. This is the state a failed Prep that started from an
// already-current target leaves behind: `--abort` and `--finalize` wait out
// their timeout, because nothing moves.
func (u IBU) StuckIdle() (guestctl.Condition, bool) {
	if u.Spec.Stage != StageIdle {
		return guestctl.Condition{}, false
	}
	c := u.Status.Conditions.Find(stageConditions[StageIdle].completed)
	if c == nil || c.Status == "True" || IdleHandled(c.Reason) {
		return guestctl.Condition{}, false
	}
	return *c, true
}

// UnstickPatch is the JSON patch that points the Idle condition at a reason
// LCA's controller acts on — Aborting, or Finalizing once an upgrade completed.
// It addresses the condition by its position, which is how json-patch reaches
// it. Applying it to an IBU StuckIdle reported, then restarting the controller,
// is the recovery.
func (u IBU) UnstickPatch() string {
	reason := "Aborting"
	if u.Status.Conditions.IsTrue(stageConditions[StageUpgrade].completed) {
		reason = "Finalizing"
	}
	idx := 0
	for i, x := range u.Status.Conditions {
		if x.Type == stageConditions[StageIdle].completed {
			idx = i
			break
		}
	}
	return fmt.Sprintf(`[{"op":"replace","path":"/status/conditions/%d/reason","value":%q}]`, idx, reason)
}

// Note is what LCA's conditions say about stage s now, as "Reason: message",
// for a timeout that would otherwise report nothing. The in-progress condition
// is the specific one when there is one, as in Toward.
func (u IBU) Note(s Stage) string {
	c := stageConditions[s]
	show := func(x *guestctl.Condition) string {
		switch {
		case x == nil:
			return ""
		case x.Reason == "":
			return x.Message
		case x.Message == "":
			return x.Reason
		}
		return x.Reason + ": " + x.Message
	}
	if c.inProgress != "" {
		if note := show(u.Status.Conditions.Find(c.inProgress)); note != "" {
			return note
		}
	}
	return show(u.Status.Conditions.Find(c.completed))
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
