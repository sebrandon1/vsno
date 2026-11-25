package precheck

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) NodeHardware {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	hw, err := ParseProbe(string(b))
	if err != nil {
		t.Fatalf("ParseProbe(%s): %v", name, err)
	}
	return hw
}

func findDisk(hw NodeHardware, name string) *Disk {
	for i := range hw.Disks {
		if hw.Disks[i].Name == name {
			return &hw.Disks[i]
		}
	}
	return nil
}

func TestParseProbeBareMetalEmptyDisk(t *testing.T) {
	hw := loadFixture(t, "probe-baremetal-empty-disk.txt")
	if !hw.BareMetal() || !hw.KVM {
		t.Fatalf("want bare metal with KVM, got virt=%q kvm=%v", hw.Virt, hw.KVM)
	}
	if hw.Threads != 104 || hw.Product != "Example Server" {
		t.Fatalf("threads=%d product=%q", hw.Threads, hw.Product)
	}
	if len(hw.Disks) != 2 {
		t.Fatalf("want 2 whole disks (loops skipped), got %d", len(hw.Disks))
	}
	sda, sdb := findDisk(hw, "sda"), findDisk(hw, "sdb")
	if sda == nil || sda.Empty() {
		t.Fatalf("sda is the OS disk and must not be empty: %+v", sda)
	}
	if sdb == nil || !sdb.Empty() {
		t.Fatalf("sdb should be empty: %+v", sdb)
	}
	if got, want := sdb.PreferredByID(), "/dev/disk/by-id/wwn-0x5000000000000002"; got != want {
		t.Fatalf("PreferredByID = %q, want %q", got, want)
	}
	if sdb.SizeGB() != 1919 {
		t.Fatalf("SizeGB = %d", sdb.SizeGB())
	}
}

func TestParseProbeBareMetalLVMSDisk(t *testing.T) {
	hw := loadFixture(t, "probe-baremetal-lvms-disk.txt")
	sdb := findDisk(hw, "sdb")
	if sdb == nil || sdb.Empty() || !sdb.LVMMember() {
		t.Fatalf("sdb should be an LVM member, not empty: %+v", sdb)
	}
	// lvm-pv-uuid aliases must never be chosen.
	if got := sdb.PreferredByID(); got != "/dev/disk/by-id/wwn-0x5000000000000002" {
		t.Fatalf("PreferredByID = %q", got)
	}
}

func TestParseProbeVMWorker(t *testing.T) {
	hw := loadFixture(t, "probe-vm-worker.txt")
	if hw.BareMetal() {
		t.Fatal("VM worker must not be reported as bare metal")
	}
	if !hw.KVM {
		t.Fatal("fixture has nested KVM exposed; KVM should be true")
	}
	vdb := findDisk(hw, "vdb")
	if vdb == nil || !vdb.Empty() || vdb.SizeGB() != 107 {
		t.Fatalf("vdb should be an empty ~107GB disk: %+v", vdb)
	}
	if findDisk(hw, "sr0") != nil {
		t.Fatal("rom devices must be skipped")
	}
}

func TestParseProbeRejectsGarbage(t *testing.T) {
	if _, err := ParseProbe("hello\nworld"); err == nil {
		t.Fatal("expected an error for output without VIRT/NPROC")
	}
}

// A disk is only empty when the probe proves it: zero value is not empty.
func TestDiskNeedsPositiveEvidence(t *testing.T) {
	var d Disk
	if d.Empty() {
		t.Fatal("a disk with no probe evidence must not be empty")
	}
	if got := strings.Join(d.Problems(), ", "); !strings.Contains(got, "not checked") {
		t.Fatalf("Problems = %q, want the unchecked fields named", got)
	}
}

// A raw Ceph OSD has no partition table and no lsblk FSTYPE; wipefs still sees
// its BlueStore label, and the first MiB holds data.
func TestParseProbeCephOSD(t *testing.T) {
	hw := loadFixture(t, "probe-ceph-osd-only.txt")
	sdb := findDisk(hw, "sdb")
	if sdb == nil {
		t.Fatal("no sdb")
	}
	if sdb.Empty() {
		t.Fatalf("a raw Ceph OSD must not be empty: %+v", sdb)
	}
	why := strings.Join(sdb.Problems(), ", ")
	for _, want := range []string{"ceph_bluestore", "the first MiB holds data"} {
		if !strings.Contains(why, want) {
			t.Errorf("Problems = %q, missing %q", why, want)
		}
	}
}

func TestParseProbeFailedLsblkIsReported(t *testing.T) {
	hw, err := ParseProbe("VIRT=none\nNPROC=8\nLSBLK_STATUS=failed\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(hw.Errors) == 0 || !strings.Contains(hw.Errors[0], "lsblk") {
		t.Fatalf("Errors = %v, want lsblk named", hw.Errors)
	}
}

// lsblk reports an unmounted device as mountpoints:[""], and a mount on a
// partition belongs to the disk.
func TestParseProbeMountpoints(t *testing.T) {
	out := `VIRT=none
NPROC=8
LSBLK={"blockdevices":[{"name":"sdb","size":1919716163584,"type":"disk","fstype":null,"pttype":null,"mountpoints":[""],"rm":false,"children":[{"name":"sdb1","size":1919716163584,"type":"part","fstype":"xfs","pttype":null,"mountpoints":["/data"],"rm":false}]}]}
BYID=sdb wwn-0x5000000000000002
LSBLK_STATUS=ok
HOSTMOUNTS_CHECKED=yes
HOLDERS=sdb -
SLAVES=sdb -
SIGCHECK=sdb ok
SIG=sdb -
DATA=sdb head=zero tail=zero`
	hw, err := ParseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	d := findDisk(hw, "sdb")
	if d == nil {
		t.Fatal("no sdb")
	}
	if want := []string{"/data"}; !slices.Equal(d.Mountpoints, want) {
		t.Fatalf("Mountpoints = %q, want %q (the empty one dropped)", d.Mountpoints, want)
	}
	if d.Empty() || !strings.Contains(strings.Join(d.Problems(), ", "), "mounted at /data") {
		t.Fatalf("a disk with a mounted partition must not be empty: %+v", d.Problems())
	}
}

// A PV whose lsblk FSTYPE is blank is still an LVM member when wipefs sees it.
func TestParseProbeLVMBySignatureOnly(t *testing.T) {
	out := `VIRT=none
NPROC=8
LSBLK={"blockdevices":[{"name":"sdb","size":1919716163584,"type":"disk","fstype":null,"pttype":null,"mountpoint":null,"rm":false}]}
LSBLK_STATUS=ok
HOSTMOUNTS_CHECKED=yes
HOLDERS=sdb -
SLAVES=sdb -
SIGCHECK=sdb ok
SIG=sdb LVM2_member
DATA=sdb head=nonzero tail=nonzero`
	hw, err := ParseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	d := findDisk(hw, "sdb")
	if d == nil || !d.LVMMember() {
		t.Fatalf("signature-only LVM member not recognized: %+v", d)
	}
}

func TestFlexTypes(t *testing.T) {
	out := `VIRT=none
NPROC=8
LSBLK={"blockdevices":[{"name":"nvme0n1","size":"1000204886016","type":"disk","fstype":null,"pttype":null,"mountpoint":null,"rm":"0"}]}
BYID=nvme0n1 nvme-eui.0025385a91b04a0b
BYID=nvme0n1 nvme-Samsung_SSD_970
LSBLK_STATUS=ok
HOSTMOUNTS_CHECKED=yes
SIGCHECK=nvme0n1 ok
SIG=nvme0n1 -
DATA=nvme0n1 head=zero tail=zero`
	hw, err := ParseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	d := findDisk(hw, "nvme0n1")
	if d == nil || !d.Empty() || d.SizeGB() != 1000 {
		t.Fatalf("unexpected disk %+v", d)
	}
	if got := d.PreferredByID(); got != "/dev/disk/by-id/nvme-eui.0025385a91b04a0b" {
		t.Fatalf("PreferredByID = %q", got)
	}
}
