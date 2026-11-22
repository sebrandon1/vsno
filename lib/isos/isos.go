// Package isos reports and removes what vSNO creates leave behind: the ISOs
// the builder serves, its build directories, the cached openshift-install
// binaries, and the ISO DataVolumes on LVMS.
package isos

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Env is what the isos commands need: the builder's files and the namespace.
type Env struct {
	guest.Env
}

// Kind is what a file in the builder is for.
type Kind string

const (
	Served    Kind = "served"    // /work/www/<vsno>.iso, the ISO a VM boots from
	Build     Kind = "build"     // /work/guests/<vsno>, the installer's directory
	Installer Kind = "installer" // /work/bin/openshift-install-<sha>
	Base      Kind = "base"      // /work/cache, the installer's base ISO
)

// Use says whether something is still needed.
type Use string

const (
	InUse    Use = "in use"   // a vSNO in the namespace uses it
	Orphaned Use = "orphaned" // its vSNO is gone
	Unused   Use = "unused"   // no vSNO uses it
	Unknown  Use = "unknown"  // can't tell, so prune leaves it alone
)

// Entry is one file in the builder.
type Entry struct {
	Kind     Kind      `json:"kind"`
	Name     string    `json:"name"`
	Path     string    `json:"path,omitempty"`    // where it is in the builder, e.g. /work/www/a.iso
	Release  string    `json:"release,omitempty"` // an installer's release image
	Version  string    `json:"version,omitempty"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified,omitempty"`
	Use      Use       `json:"use"`
	Users    []string  `json:"users,omitempty"` // the vSNOs that use it
}

// Disk is an ISO DataVolume in the namespace.
type Disk struct {
	Name  string   `json:"name"`
	VSNO  string   `json:"vsno"`
	Size  int64    `json:"size"`
	Use   Use      `json:"use"`
	Users []string `json:"users,omitempty"`
}

// Report is what `vsno isos` shows.
type Report struct {
	Entries []Entry `json:"entries"`
	// Used and Total are the builder volume's.
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
	// Disks are the ISO DataVolumes, and DiskBytes what they hold.
	Disks     []Disk `json:"disks,omitempty"`
	DiskBytes int64  `json:"diskBytes,omitempty"`
}

//go:embed scripts/*.sh
var files embed.FS

func script(name string) string {
	b, err := files.ReadFile("scripts/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}

// line is one object from isos.sh; each kind fills the fields it has.
type line struct {
	Kind     Kind      `json:"kind"`
	Name     string    `json:"name"`
	Release  string    `json:"release"`
	Version  string    `json:"version"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Used     int64     `json:"used"`
	Total    int64     `json:"total"`
}

// List reports what the builder holds and what still uses it. A missing
// builder means no files: the report then only covers the namespace.
func List(ctx context.Context, e Env, ns string) (*Report, error) {
	rep := &Report{}
	lines, err := list(ctx, e)
	if err != nil {
		return nil, err
	}
	vms, err := vsnos(ctx, e, ns)
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		switch l.Kind {
		case volume:
			rep.Used, rep.Total = l.Used, l.Total
		case Base:
			rep.Entries = append(rep.Entries, Entry{Kind: Base, Name: l.Name, Path: pathOf(Base, l.Name), Size: l.Size, Modified: l.Modified})
		default:
			rep.Entries = append(rep.Entries, entryFor(l, vms))
		}
	}
	rep.Disks, rep.DiskBytes, err = disks(ctx, e, ns, vms)
	return rep, err
}

// volume is the line kind that carries the builder volume's usage.
const volume Kind = "volume"

// pathOf is where an entry of this kind lives in the builder.
func pathOf(k Kind, name string) string {
	switch k {
	case Served:
		return "/work/www/" + name + ".iso"
	case Build:
		return "/work/guests/" + name
	case Installer:
		return "/work/bin/" + name
	case Base:
		return "/work/cache"
	}
	return ""
}

// entryFor classifies one served ISO, build directory or installer against
// the vSNOs in the namespace.
func entryFor(l line, vms map[string]string) Entry {
	ent := Entry{Kind: l.Kind, Name: l.Name, Path: pathOf(l.Kind, l.Name), Release: l.Release, Version: l.Version, Size: l.Size, Modified: l.Modified}
	if l.Kind == Installer {
		ent.Users = users(vms, l.Release)
		switch {
		case l.Release == "":
			ent.Use = Unknown // an older binary, with no .ref to match on
		case len(ent.Users) > 0:
			ent.Use = InUse
		default:
			ent.Use = Unused
		}
		return ent
	}
	if _, ok := vms[l.Name]; ok {
		ent.Use, ent.Users = InUse, []string{l.Name}
	} else {
		ent.Use = Orphaned
	}
	return ent
}

// list runs isos.sh and parses its lines. Without a builder, there are none.
func list(ctx context.Context, e Env) ([]line, error) {
	if e.Builder == nil {
		return nil, nil
	}
	var out, stderr bytes.Buffer
	if err := e.Builder.Run(ctx, script("isos.sh"), nil, builder.IO{Stdout: &out, Stderr: &stderr}); err != nil {
		return nil, fmt.Errorf("listing the builder's files: %w%s", err, builder.Tail(stderr.String()))
	}
	var lines []line
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("reading the builder's file list: %w", err)
		}
		lines = append(lines, l)
	}
	return lines, sc.Err()
}

// vsnos returns the vSNOs in ns, by name, with the release image they run.
func vsnos(ctx context.Context, e Env, ns string) (map[string]string, error) {
	l, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil // no KubeVirt
	}
	if err != nil {
		return nil, fmt.Errorf("listing vSNOs: %w", err)
	}
	out := make(map[string]string, len(l.Items))
	for i := range l.Items {
		out[l.Items[i].GetName()] = l.Items[i].GetAnnotations()[guest.AnnImage]
	}
	return out, nil
}

// users lists the vSNOs running the release image ref.
func users(vms map[string]string, ref string) []string {
	var out []string
	for name, image := range vms {
		if ref != "" && image == ref {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// disks lists the <vsno>-iso DataVolumes in ns.
func disks(ctx context.Context, e Env, ns string, vms map[string]string) ([]Disk, int64, error) {
	l, err := e.Dyn.Resource(guest.GVRDataVolume).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("listing the ISO DataVolumes: %w", err)
	}
	var out []Disk
	var total int64
	for i := range l.Items {
		name := l.Items[i].GetName()
		vsno, ok := strings.CutSuffix(name, "-iso")
		if !ok {
			continue
		}
		d := Disk{Name: name, VSNO: vsno, Size: size(l.Items[i])}
		if _, ok := vms[vsno]; ok {
			d.Use, d.Users = InUse, []string{vsno}
		} else {
			d.Use = Orphaned
		}
		total += d.Size
		out = append(out, d)
	}
	return out, total, nil
}

// size is the DataVolume's requested storage in bytes.
func size(u unstructured.Unstructured) int64 {
	s, _, _ := unstructured.NestedString(u.Object, "spec", "storage", "resources", "requests", "storage")
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}
