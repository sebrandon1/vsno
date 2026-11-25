package precheck

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ProbeScript runs on a node (via chroot /host) and prints KEY=VALUE lines.
//
//go:embed probe.sh
var ProbeScript string

// NodeHardware is the parsed output of ProbeScript.
type NodeHardware struct {
	Product        string
	Virt           string // systemd-detect-virt output; "none" on bare metal
	KVM            bool
	Threads        int
	MemTotalKB     int64
	MemAvailableKB int64
	Disks          []Disk
	// Errors are parts of the probe that could not run, so a degraded probe is
	// reported rather than read as "the node has no disks".
	Errors []string
}

// BareMetal reports whether the node is not itself a VM.
func (h NodeHardware) BareMetal() bool {
	return h.Virt == "none"
}

// Disk is a whole block device on the node.
type Disk struct {
	Name        string
	SizeBytes   int64
	FSType      string
	PTType      string
	Mountpoints []string // mounted in the probe pod's namespace
	Removable   bool
	Partitions  int
	ByID        []string // /dev/disk/by-id names pointing at this disk

	// The fields below come from the probe's second opinion. A disk is only
	// empty when they prove it, never because the obvious fields are blank.
	Signatures        []string // every signature wipefs -n reports
	Holders           []string // /sys/block/<dev>/holders, and the partitions'
	Slaves            []string // /sys/block/<dev>/slaves, and the partitions'
	HostMountpoints   []string // mounted on the node (host mount namespace)
	SigChecked        bool     // the signatures were actually checked
	HostMountsChecked bool     // the node's mount table was actually read
	DataHead          DataState
	DataTail          DataState
	// PVName is the local PersistentVolume that references this disk, if any.
	PVName  string
	PVBound bool // something claims that PV
}

// DataState is what a bounded read of a disk found.
type DataState string

const (
	DataUnknown DataState = ""        // could not be read: not evidence of emptiness
	DataZero    DataState = "zero"    // read succeeded and was all zeros
	DataNonZero DataState = "nonzero" // read succeeded and found data
)

// Problems lists everything that keeps the disk from being provably blank. A
// disk is empty only when this is empty: anything unproven is not empty.
func (d Disk) Problems() []string {
	var p []string
	if d.PVName != "" {
		if d.PVBound {
			p = append(p, "claimed by local PV "+d.PVName)
		} else {
			p = append(p, "referenced by local PV "+d.PVName)
		}
	}
	if d.Removable {
		p = append(p, "removable")
	}
	if d.Partitions > 0 {
		p = append(p, fmt.Sprintf("%d partition(s) or stacked device(s)", d.Partitions))
	}
	if d.PTType != "" {
		p = append(p, "partition table "+d.PTType)
	}
	if d.FSType != "" {
		p = append(p, "filesystem "+d.FSType)
	}
	if extra := otherSignatures(d); len(extra) > 0 {
		p = append(p, "signatures "+strings.Join(extra, ","))
	}
	if len(d.Holders) > 0 {
		p = append(p, "held by "+strings.Join(d.Holders, ","))
	}
	if len(d.Slaves) > 0 {
		p = append(p, "backed by "+strings.Join(d.Slaves, ","))
	}
	for _, mp := range d.Mountpoints {
		p = append(p, "mounted at "+mp)
	}
	for _, mp := range d.HostMountpoints {
		p = append(p, "mounted at "+mp+" on the node")
	}
	if !d.SigChecked {
		p = append(p, "signatures were not checked")
	}
	if !d.HostMountsChecked {
		p = append(p, "node mounts were not checked")
	}
	return append(p, dataProblems(d)...)
}

// otherSignatures drops the signature lsblk already reports as the filesystem.
func otherSignatures(d Disk) []string {
	var extra []string
	for _, s := range d.Signatures {
		if s != d.FSType {
			extra = append(extra, s)
		}
	}
	return extra
}

func dataProblems(d Disk) []string {
	var p []string
	for _, x := range []struct {
		what string
		have DataState
	}{{"the first MiB", d.DataHead}, {"the last 4 KiB", d.DataTail}} {
		switch x.have {
		case DataZero:
		case DataNonZero:
			p = append(p, x.what+" holds data")
		default:
			p = append(p, x.what+" could not be read")
		}
	}
	return p
}

// Empty reports whether the probe could prove the disk is blank.
func (d Disk) Empty() bool { return len(d.Problems()) == 0 }

// LVMMember reports whether the disk is an LVM physical volume (e.g. already taken by LVMS).
func (d Disk) LVMMember() bool {
	return d.FSType == "LVM2_member" || slices.Contains(d.Signatures, "LVM2_member")
}

// SizeGB is the size in decimal gigabytes.
func (d Disk) SizeGB() int64 {
	return d.SizeBytes / 1_000_000_000
}

// PreferredByID returns the most stable /dev/disk/by-id path for the disk:
// wwn-* first, then scsi-3*/nvme-eui.*, then anything except LVM/DM aliases.
func (d Disk) PreferredByID() string {
	rank := func(id string) int {
		switch {
		case strings.HasPrefix(id, "wwn-"):
			return 0
		case strings.HasPrefix(id, "nvme-eui."), strings.HasPrefix(id, "scsi-3"):
			return 1
		case strings.HasPrefix(id, "lvm-pv-uuid-"), strings.HasPrefix(id, "dm-"):
			return 9
		default:
			return 2
		}
	}
	best, bestRank := "", 99
	ids := append([]string(nil), d.ByID...)
	sort.Strings(ids)
	for _, id := range ids {
		if r := rank(id); r < bestRank {
			best, bestRank = id, r
		}
	}
	if best == "" || bestRank == 9 {
		return ""
	}
	return "/dev/disk/by-id/" + best
}

// lsblk -J field types vary across util-linux versions (bool vs "0"/"1", number vs string).
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	*b = flexBool(s == "true" || s == "1")
	return nil
}

type flexInt int64

func (n *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("parsing size %q: %w", s, err)
	}
	*n = flexInt(v)
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type lsblkDevice struct {
	Name        string        `json:"name"`
	Size        flexInt       `json:"size"`
	Type        string        `json:"type"`
	FSType      *string       `json:"fstype"`
	PTType      *string       `json:"pttype"`
	Mountpoint  *string       `json:"mountpoint"`
	Mountpoints []string      `json:"mountpoints"`
	RM          flexBool      `json:"rm"`
	Children    []lsblkDevice `json:"children"`
}

// mounts is the device's mountpoints, from MOUNTPOINTS when lsblk has it and
// from MOUNTPOINT otherwise. lsblk reports unmounted devices as [""].
func (dev lsblkDevice) mounts() []string {
	var mps []string
	if len(dev.Mountpoints) > 0 {
		mps = dev.Mountpoints
	} else if mp := deref(dev.Mountpoint); mp != "" {
		mps = []string{mp}
	}
	kept := make([]string, 0, len(mps))
	for _, mp := range mps {
		if mp != "" {
			kept = append(kept, mp)
		}
	}
	return kept
}

// probeDiskState collects the per-disk second opinion between key lines.
type probeDiskState struct {
	byID              map[string][]string
	signatures        map[string][]string
	holders           map[string][]string
	slaves            map[string][]string
	hostMounts        map[string][]string
	data              map[string][2]DataState
	sigChecked        map[string]bool
	hostMountsChecked bool
}

func newProbeDiskState() *probeDiskState {
	return &probeDiskState{
		byID:       map[string][]string{},
		signatures: map[string][]string{},
		holders:    map[string][]string{},
		slaves:     map[string][]string{},
		hostMounts: map[string][]string{},
		data:       map[string][2]DataState{},
		sigChecked: map[string]bool{},
	}
}

// csvList turns "a,b" into [a b]; "-" and "" mean nothing.
func csvList(s string) []string {
	if s == "" || s == "-" {
		return nil
	}
	return strings.Split(s, ",")
}

// devAndRest splits "<device> <rest>" as the per-disk lines are written.
func devAndRest(val string) (dev, rest string, ok bool) {
	dev, rest, ok = strings.Cut(val, " ")
	return dev, strings.TrimSpace(rest), ok
}

// ParseProbe parses ProbeScript output.
func ParseProbe(out string) (NodeHardware, error) {
	var hw NodeHardware
	st := newProbeDiskState()
	var lsblkJSON string
	lsblkOK := true

	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LSBLK":
			lsblkJSON = val
			continue
		case "LSBLK_STATUS":
			lsblkOK = val == "ok"
			continue
		case "HOSTMOUNTS_CHECKED":
			st.hostMountsChecked = val == "yes"
			continue
		}
		applyProbeLine(&hw, st, key, val)
	}

	if hw.Virt == "" || hw.Threads == 0 {
		return hw, fmt.Errorf("probe output is missing VIRT/NPROC; got %d bytes", len(out))
	}
	if !lsblkOK {
		hw.Errors = append(hw.Errors, "lsblk failed; no disks could be read")
		return hw, nil
	}
	if lsblkJSON == "" {
		return hw, nil
	}
	disks, err := parseLsblk(lsblkJSON, st)
	if err != nil {
		return hw, err
	}
	hw.Disks = disks
	return hw, nil
}

// applyProbeLine handles the per-disk and error lines; ParseProbe takes the
// rest before they reach here.
func applyProbeLine(hw *NodeHardware, st *probeDiskState, key, val string) {
	switch key {
	case "BYID":
		addDeviceValue(st.byID, val)
	case "SIG":
		addDeviceValue(st.signatures, val)
	case "HOLDERS":
		addDeviceValue(st.holders, val)
	case "SLAVES":
		addDeviceValue(st.slaves, val)
	case "HOSTMOUNT":
		addDeviceValue(st.hostMounts, val)
	case "SIGCHECK":
		if dev, rest, ok := devAndRest(val); ok {
			st.sigChecked[dev] = rest == "ok"
		}
	case "DATA":
		if dev, rest, ok := devAndRest(val); ok {
			st.data[dev] = parseData(rest)
		}
	case "PROBEERR":
		hw.Errors = append(hw.Errors, val)
	default:
		applyScalarField(hw, key, val)
	}
}

// addDeviceValue appends a "<device> <csv>" line's values to m.
func addDeviceValue(m map[string][]string, val string) {
	if dev, rest, ok := devAndRest(val); ok {
		m[dev] = append(m[dev], csvList(rest)...)
	}
}

// parseData reads "head=zero tail=nonzero"; anything else is unknown.
func parseData(rest string) [2]DataState {
	var out [2]DataState
	for _, f := range strings.Fields(rest) {
		v, ok := strings.CutPrefix(f, "head=")
		if ok {
			out[0] = DataState(v)
			continue
		}
		if v, ok := strings.CutPrefix(f, "tail="); ok {
			out[1] = DataState(v)
		}
	}
	return out
}

func applyScalarField(hw *NodeHardware, key, val string) {
	switch key {
	case "PRODUCT":
		hw.Product = val
	case "VIRT":
		hw.Virt = val
	case "KVM":
		hw.KVM = val == "yes"
	case "NPROC":
		hw.Threads, _ = strconv.Atoi(val)
	case "MEM_TOTAL_KB":
		hw.MemTotalKB, _ = strconv.ParseInt(val, 10, 64)
	case "MEM_AVAILABLE_KB":
		hw.MemAvailableKB, _ = strconv.ParseInt(val, 10, 64)
	}
}

// parseLsblk turns `lsblk -J` output into whole disks (loop/rom devices are skipped).
func parseLsblk(js string, st *probeDiskState) ([]Disk, error) {
	var parsed struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(js), &parsed); err != nil {
		return nil, fmt.Errorf("parsing lsblk output: %w", err)
	}
	var disks []Disk
	for _, dev := range parsed.BlockDevices {
		// Zero-size devices are placeholders (nbd without a backend); they can
		// never hold anything and only clutter the report.
		if dev.Type != "disk" || dev.Size == 0 {
			continue
		}
		d := Disk{
			Name:              dev.Name,
			SizeBytes:         int64(dev.Size),
			FSType:            deref(dev.FSType),
			PTType:            deref(dev.PTType),
			Mountpoints:       dev.mounts(),
			Removable:         bool(dev.RM),
			Partitions:        len(dev.Children),
			ByID:              st.byID[dev.Name],
			Signatures:        st.signatures[dev.Name],
			Holders:           st.holders[dev.Name],
			Slaves:            st.slaves[dev.Name],
			HostMountpoints:   st.hostMounts[dev.Name],
			SigChecked:        st.sigChecked[dev.Name],
			HostMountsChecked: st.hostMountsChecked,
		}
		for _, child := range dev.Children {
			d.Mountpoints = append(d.Mountpoints, child.mounts()...)
		}
		if data, ok := st.data[dev.Name]; ok {
			d.DataHead, d.DataTail = data[0], data[1]
		}
		disks = append(disks, d)
	}
	return disks, nil
}
