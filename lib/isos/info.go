package isos

import "fmt"

// Info is what `vsno iso info` shows for one name: every builder file and ISO
// DataVolume it owns, and the builder volume's usage.
type Info struct {
	Name    string  `json:"name"`
	Entries []Entry `json:"entries,omitempty"`
	Disks   []Disk  `json:"disks,omitempty"`
	// Bytes is everything the name holds, in the builder and on LVMS.
	Bytes int64 `json:"bytes"`
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// DiskBytes is what the name's ISO DataVolumes hold on LVMS; the rest of Bytes
// is in the builder.
func (in *Info) DiskBytes() int64 {
	var n int64
	for _, d := range in.Disks {
		n += d.Size
	}
	return n
}

// Find picks what a name refers to out of a listing. A vSNO brings its served
// ISO, its build directory and its ISO DataVolume; an installer or a
// DataVolume brings itself, by its own name or by its vSNO's. Unlike Delete,
// nothing in use is refused: that is what info exists for.
func Find(rep *Report, name string) (*Info, error) {
	in := &Info{Name: name, Used: rep.Used, Total: rep.Total}
	for _, ent := range rep.Entries {
		if ent.Name != name {
			continue
		}
		in.Entries = append(in.Entries, ent)
		in.Bytes += ent.Size
	}
	for _, d := range rep.Disks {
		if d.Name != name && d.VSNO != name {
			continue
		}
		in.Disks = append(in.Disks, d)
		in.Bytes += d.Size
	}
	if len(in.Entries) == 0 && len(in.Disks) == 0 {
		return nil, fmt.Errorf("nothing named %s in the builder or the ISO DataVolumes (see `vsno iso list`)", name)
	}
	return in, nil
}
