#!/bin/bash
# vsno hardware probe. Runs on the node (chroot /host). Prints KEY=VALUE lines that
# lib/precheck parses. Read-only: no writes to the node.
echo "PRODUCT=$(cat /sys/class/dmi/id/product_name 2>/dev/null | tr -d '\n')"
echo "VIRT=$(systemd-detect-virt 2>/dev/null || true)"
if [ -c /dev/kvm ]; then echo "KVM=yes"; else echo "KVM=no"; fi
echo "NPROC=$(nproc)"
echo "MEM_TOTAL_KB=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)"
echo "MEM_AVAILABLE_KB=$(awk '/^MemAvailable:/{print $2}' /proc/meminfo)"

# MOUNTPOINTS needs util-linux 2.37+; older lsblk only knows MOUNTPOINT.
if lsblk -J -o NAME,MOUNTPOINTS >/dev/null 2>&1; then MP=MOUNTPOINTS; else MP=MOUNTPOINT; fi
LSBLK_JSON=$(lsblk -J -b -o NAME,SIZE,TYPE,FSTYPE,PTTYPE,$MP,ROTA,RM 2>/dev/null | tr -d '\n')
if [ -n "$LSBLK_JSON" ]; then echo "LSBLK_STATUS=ok"; else echo "LSBLK_STATUS=failed"; fi
echo "LSBLK=$LSBLK_JSON"

# The probe pod is hostPID, so PID 1 is the node's init and /proc/1/mountinfo is
# the node's mount table. lsblk reads /proc/self/mountinfo, i.e. the pod's mount
# namespace, where a disk mounted on the node can look unmounted.
HOSTMOUNTS=no
if [ "$(cat /proc/1/comm 2>/dev/null)" = "systemd" ]; then HOSTMOUNTS=yes; fi
echo "HOSTMOUNTS_CHECKED=$HOSTMOUNTS"

for l in /dev/disk/by-id/*; do
  [ -L "$l" ] || continue
  echo "BYID=$(basename "$(readlink -f "$l")") $(basename "$l")"
done

# Emits a sorted, comma-separated list, or nothing.
csv() { sort -u | tr '\n' ',' | sed 's/,$//'; }

# Names under /sys/block/<dev>/<kind> for the disk and each of its partitions.
# One directory per ls call: ls prints a "path:" header for several operands.
syslist() {
  for p in /sys/block/$1/$2 /sys/block/$1/$1*/$2; do [ -d "$p" ] && ls "$p"; done | csv
}

for dev in $(lsblk -dno NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}'); do
  d=/dev/$dev
  # Holders and slaves catch dm-multipath, dm-crypt, LVM and MD.
  h=$(syslist "$dev" holders)
  s=$(syslist "$dev" slaves)
  echo "HOLDERS=$dev ${h:--}"
  echo "SLAVES=$dev ${s:--}"

  # Every signature on the whole disk. lsblk may hide one when libblkid finds
  # several (a raw Ceph OSD carries a BlueStore label plus XFS log leftovers and
  # lsblk then prints no FSTYPE at all), so ask wipefs directly. -n never writes.
  wout=$(wipefs -n "$d" 2>/dev/null); wrc=$?
  sigs=$(printf '%s\n' "$wout" | awk '$3 != "TYPE" && NF >= 3 {print $3}' | csv)
  if [ "$wrc" -eq 0 ] && command -v wipefs >/dev/null 2>&1; then
    echo "SIGCHECK=$dev ok"
  else
    echo "SIGCHECK=$dev unavailable"
  fi
  echo "SIG=$dev ${sigs:--}"

  # Mounted anywhere on the node, the disk itself or one of its partitions.
  if [ "$HOSTMOUNTS" = yes ]; then
    mp=$(for mm in $(lsblk -ln -o MAJ:MIN "$d" 2>/dev/null); do
           awk -v m="$mm" '$3==m {print $5}' /proc/1/mountinfo 2>/dev/null
         done | csv)
    [ -n "$mp" ] && echo "HOSTMOUNT=$dev $mp"
  fi

  # A disk is only blank if it reads as zeros where no signature says otherwise.
  size=$(blockdev --getsize64 "$d" 2>/dev/null || echo 0)
  head=unknown; tail=unknown
  if [ "$size" -gt 0 ] 2>/dev/null; then
    cmp -n 1048576 "$d" /dev/zero >/dev/null 2>&1; rc=$?
    case $rc in 0) head=zero;; 1) head=nonzero;; *) head=unknown;; esac
    dd if="$d" bs=4096 skip=$(( size/4096 - 1 )) count=1 status=none 2>/dev/null | cmp -s - /dev/zero; rc=$?
    case $rc in 0) tail=zero;; 1) tail=nonzero;; *) tail=unknown;; esac
  fi
  echo "DATA=$dev head=$head tail=$tail"
done
