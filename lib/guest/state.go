package guest

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// State is where a vSNO is in its life.
type State string

const (
	Building   State = "building-iso"  // create is building the agent ISO; no VM yet
	Importing  State = "importing-iso" // CDI is copying the ISO into the cluster
	Starting   State = "starting"      // the VM is scheduling or booting
	Installing State = "installing"    // the agent installer runs, before the first reboot
	Finishing  State = "finishing"     // the guest API is up, ClusterVersion not yet Available
	Ready      State = "ready"
	Stopped    State = "stopped"
	Failed     State = "failed"
	Unknown    State = "unknown" // running, but nothing answers yet (rebooting, or the builder is down)
	Deleting   State = "deleting"
)

// Probe is one line of the builder's probe.sh.
type Probe struct {
	Name      string `json:"name"`
	Agent     *Agent `json:"agent"`
	CV        *CV    `json:"cv"`
	Unhealthy *int   `json:"unhealthy"`
	IBU       *IBU   `json:"ibu"`
}

// IBU is where LCA's Image Based Upgrade on the guest is, when LCA runs.
type IBU struct {
	Stage     string `json:"stage"`
	Completed bool   `json:"completed"`
}

// Text is the stage, marked done once it completed, or "" while the guest is
// idle: nothing to show.
func (u *IBU) Text() string {
	switch {
	case u == nil || u.Stage == "" || u.Stage == "Idle":
		return ""
	case u.Completed:
		return u.Stage + " done"
	}
	return u.Stage
}

// Agent is the agent installer's view of the cluster, before the first reboot.
type Agent struct {
	Status   string `json:"status"`
	Info     string `json:"info"`
	Progress int    `json:"progress"`
	// Validations is the installer's validations_info: a JSON string mapping
	// a category to its validations.
	Validations string `json:"validations"`
}

// CV is the guest's ClusterVersion.
type CV struct {
	Version   string `json:"version"`
	Available bool   `json:"available"`
	Message   string `json:"message"`
}

// Failing returns the IDs of validations that failed, sorted.
func (a *Agent) Failing() []string {
	var v map[string][]struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if a == nil || json.Unmarshal([]byte(a.Validations), &v) != nil {
		return nil
	}
	var out []string
	for _, list := range v {
		for _, x := range list {
			if x.Status == "failure" {
				out = append(out, x.ID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Observed is what Kubernetes and the builder report about one vSNO.
type Observed struct {
	// VMStatus is the VM's status.printableStatus (Stopped, Provisioning, Running, ErrorUnschedulable...).
	VMStatus string
	// WantsRunning is true when the VM's spec says it should run. Right
	// after it is created, KubeVirt reports such a VM Stopped for a moment.
	WantsRunning bool
	// ISOPhase and ISOProgress are the ISO DataVolume's status.phase and status.progress.
	ISOPhase, ISOProgress string
	// Probe is nil when the guest wasn't probed (not running, or no builder).
	Probe *Probe
}

// vmStopped is true while KubeVirt reports the VM stopped or stopping.
func (o Observed) vmStopped() bool { return o.VMStatus == "Stopped" || o.VMStatus == "Stopping" }

// vmErrors are printable statuses that mean the VM can't run as is.
var vmErrors = []string{"ErrorUnschedulable", "ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "ErrorPvcNotFound", "DataVolumeError", "ErrorDataVolumeNotFound", "Unknown"}

// agentFailed are agent installer statuses the install doesn't recover from.
var agentFailed = []string{"error", "cancelled"} //nolint:misspell // the installer's spelling

// Classify turns observations into a State and a short detail.
func Classify(o Observed) (State, string) {
	switch {
	case o.VMStatus == "Terminating":
		return Deleting, ""
	case o.vmStopped() && !o.WantsRunning:
		return Stopped, ""
	case slices.Contains(vmErrors, o.VMStatus):
		return Failed, "VM " + o.VMStatus
	case o.ISOPhase == "Failed":
		return Failed, "ISO import failed"
	case o.ISOPhase != "" && o.ISOPhase != "Succeeded":
		if p := strings.TrimSpace(o.ISOProgress); p != "N/A" {
			return Importing, p
		}
		return Importing, ""
	case o.vmStopped():
		return Starting, "" // meant to run, and about to
	case o.VMStatus != "Running":
		return Starting, o.VMStatus
	case o.Probe == nil:
		return Unknown, "not probed"
	}
	return classifyProbe(o.Probe)
}

// classifyProbe classifies a running vSNO by what its installer and API report.
func classifyProbe(p *Probe) (State, string) {
	switch {
	case p.CV != nil && p.CV.Available:
		if p.Unhealthy != nil && *p.Unhealthy > 0 {
			return Ready, fmt.Sprintf("%d operator(s) degraded or unavailable", *p.Unhealthy)
		}
		return Ready, ""
	case p.CV != nil:
		return Finishing, p.CV.Message
	case p.Agent == nil:
		return Unknown, "no answer from the installer or API"
	case slices.Contains(agentFailed, p.Agent.Status):
		return Failed, "install " + p.Agent.Status + ": " + p.Agent.Info
	case p.Agent.Status == "insufficient":
		return Installing, "waiting on validations: " + strings.Join(p.Agent.Failing(), ", ")
	default:
		return Installing, fmt.Sprintf("%s %d%%", p.Agent.Status, p.Agent.Progress)
	}
}
