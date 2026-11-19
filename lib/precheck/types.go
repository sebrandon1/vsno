// Package precheck decides whether an OpenShift cluster can host vSNO (virtual
// single-node OpenShift) VMs for Image Based Upgrade testing, and suggests the
// settings `vsno setup` needs.
package precheck

// Status is the outcome of a single check.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Info Status = "info"
)

// Result is one row of the precheck report.
type Result struct {
	Check  string `json:"check"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	// Hint tells the user what to do about a warn/fail. Optional.
	Hint string `json:"hint,omitempty"`
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
	Results   []Result  `json:"results"`
	Suggested Suggested `json:"suggested"`
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
