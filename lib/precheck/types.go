// Package precheck decides whether an OpenShift cluster can host vSNO (virtual
// single-node OpenShift) VMs for Image Based Upgrade testing, and suggests the
// settings `vsno setup` needs.
package precheck

import "strings"

// Status is the outcome of a single check.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Info Status = "info"
)

// Group is the report section a result belongs to.
type Group string

const (
	GroupCluster Group = "cluster" // access, version, networks, DNS, hub
	GroupInstall Group = "install" // operator catalog, what is already installed, seed registry
	GroupWorkers Group = "workers" // worker candidates, per-node hardware, the chosen node
)

// Result is one row of the precheck report.
type Result struct {
	Check  string `json:"check"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	// Hint tells the user what to do about a warn/fail. Optional.
	Hint  string `json:"hint,omitempty"`
	Group Group  `json:"group"`
	// Node is set on per-node rows; Check is then the aspect (cpu, memory...).
	Node string `json:"node,omitempty"`
}

// Name is the row's full name: "<node>: <check>" for per-node rows.
func (r Result) Name() string {
	if r.Node == "" {
		return r.Check
	}
	return r.Node + ": " + r.Check
}

// groupOf places a result in its report section.
func groupOf(r Result) Group {
	switch {
	case r.Node != "", r.Check == "Worker nodes", r.Check == "Hardware probe", r.Check == "Eligible node":
		return GroupWorkers
	case strings.HasPrefix(r.Check, "Catalog: "), strings.HasPrefix(r.Check, "Existing: "), r.Check == "Seed registry":
		return GroupInstall
	default:
		return GroupCluster
	}
}

// NodeSummary is the verdict for one evaluated worker.
type NodeSummary struct {
	Name     string `json:"name"`
	Eligible bool   `json:"eligible"`
}

// Suggested holds the values `vsno setup` needs, derived from the checks.
type Suggested struct {
	Node        string `json:"node,omitempty"`
	Disk        string `json:"disk,omitempty"`
	DiskSizeGB  int64  `json:"diskSizeGB,omitempty"`
	HostVersion string `json:"hostVersion,omitempty"`
	HostDNS     string `json:"hostDNS,omitempty"`
	LVMSChannel string `json:"lvmsChannel,omitempty"`
	CNVChannel  string `json:"cnvChannel,omitempty"`
}

// Report is the full precheck output.
type Report struct {
	Results []Result `json:"results"`
	// Nodes are the evaluated workers, eligible first, best first.
	Nodes []NodeSummary `json:"nodes,omitempty"`
	// HubManaged is true when an ACM klusterlet manages this cluster, whose
	// policies can revert what vsno installs or upgrades.
	HubManaged bool      `json:"hubManaged,omitempty"`
	Suggested  Suggested `json:"suggested"`
}

// Failed counts results with Status Fail.
func (r *Report) Failed() int {
	n := 0
	for _, res := range r.Results {
		if res.Status == Fail {
			n++
		}
	}
	return n
}

func (r *Report) add(res Result) {
	if res.Group == "" {
		res.Group = groupOf(res)
	}
	r.Results = append(r.Results, res)
}

// Requirements describes the vSNO environment the cluster must be able to host.
type Requirements struct {
	VMs       int   // VMs running at the same time (seed + target = 2)
	VMCores   int   // vCPUs per VM
	VMMemGiB  int64 // guest memory per VM
	MinDiskGB int64 // minimum size of the disk handed to LVMS

	// Networks the guests use. They must not overlap the host's networks.
	GuestMachineCIDR string
	GuestClusterCIDR string
	GuestServiceCIDR string

	// SeedRepo, when set, is checked for push credentials in the local auth files.
	SeedRepo string
}

// DefaultRequirements matches the layout verified end to end on 2026-11-18.
func DefaultRequirements() Requirements {
	return Requirements{
		VMs:              2,
		VMCores:          16,
		VMMemGiB:         32,
		MinDiskGB:        600,
		GuestMachineCIDR: "10.0.2.0/24",
		GuestClusterCIDR: "10.132.0.0/14",
		GuestServiceCIDR: "172.31.0.0/16",
	}
}

// vmOverheadGiB approximates virt-launcher memory overhead per VM.
const vmOverheadGiB = 1

// Progress displays long-running steps. Env.Progress may be nil (silent).
type Progress interface {
	Start(title string) ProgressStep
}

// ProgressStep is one running step.
type ProgressStep interface {
	Update(detail string)
	SetProgress(done, total int)
	Done(msg string)
	Warn(msg string)
	Fail(msg string)
}

// NoProgress returns a Progress that displays nothing.
func NoProgress() Progress { return noopProgress{} }

type noopProgress struct{}
type noopStep struct{}

func (noopProgress) Start(string) ProgressStep { return noopStep{} }
func (noopStep) Update(string)                 {}
func (noopStep) SetProgress(int, int)          {}
func (noopStep) Done(string)                   {}
func (noopStep) Warn(string)                   {}
func (noopStep) Fail(string)                   {}
