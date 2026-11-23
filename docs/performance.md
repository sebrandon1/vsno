# Going faster: snapshots, caches and cleanup

An install is the slow part of a repeat test, and the registry and builder accumulate what previous
installs left. This page covers restoring an installed vSNO from a snapshot, mirroring releases into
the environment's registry, and pruning what nothing uses. Every flag is in the
[command reference](commands.md).

## Snapshots

```bash
vsno snapshot create target             # stop target, snapshot its disk, start it again
vsno snapshot list                      # what is kept, and whether its vSNO still exists
vsno create target --from-snapshot -w   # bring target back in minutes, not 17
vsno snapshot delete target --yes       # free the space
```

`snapshot` keeps a vSNO's installed root disk as a `VolumeSnapshot` and `create <name>
--from-snapshot` restores it, so a repeat test does not install OpenShift again. On LVMS the
volumes are thin, so the storage driver restores a snapshot as a thin clone in **seconds** —
measured on cnfdt16: a 10 GiB volume snapshotted in 9s and restored in 3s, verified byte for byte —
and a vSNO whose disk is restored is **ready in about a minute** (61s and 63s measured, node up and
every cluster operator green) against ~17 minutes installing.
(CDI's `DataVolume` snapshot source copies the volume host-assisted instead and took minutes for
the same 10 GiB, which is why a restore here is a PVC with a `VolumeSnapshot` dataSource.)

Two things to know:

- **Same name only.** A disk carries the cluster's identity — node name, `api.<name>.<domain>` and
  its certificates, clusterID — so it only runs as the vSNO it was taken from. Snapshots are named
  after their vSNO, and `--from-snapshot` takes no other name.
- **The vSNO stops.** A snapshot of a running guest needs the guest agent to freeze its filesystems,
  which it cannot do inside a vSNO (`failed to open /var/lib/containers: Permission denied`), so
  `snapshot create` stops the vSNO, snapshots it, and starts it again.

`vsno delete` does not remove the snapshot: that is what makes the restore possible. The installer
files are copied to `/work/snapshots/<name>` with it, so the probe can read the guest's kubeconfig
after a delete.

## ISOs and caches

```bash
vsno iso list                      # what the builder and LVMS hold, and what uses it
vsno iso info seed2                # one thing: paths, size, age, release, and its users
vsno iso delete old-vsno           # remove one vSNO's leftover ISO and build directory
vsno iso delete openshift-install-1a2b3c4d
vsno iso prune --dry-run           # ISOs, installers and DataVolumes nothing uses
vsno iso prune --yes               # ...and remove them
vsno iso prune --keep 2            # keep the two newest unused installers
vsno iso eject a                   # drop a's install ISO, if create didn't
```

`iso` (`list` is the default) lists the served ISOs, the installer's build directories, the cached `openshift-install`
binaries (one per release image), the installer's base ISO cache, and the ISO DataVolumes, each
marked in use, orphaned (its vSNO is gone) or unused. It ends with the builder volume's usage, which
`vsno status` also reports, warning above 80%.

`iso delete` removes the ISOs, installers or DataVolumes you name, asking "are you sure?" first
unless `--yes`. It refuses anything a vSNO still uses: `iso eject` takes an install ISO off the VM
safely, and `iso prune` clears the rest.

The builder volume fills up with things a finished create no longer needs: `iso.sh` keeps one copy
of each ISO rather than two, `create` ejects the ISO once the vSNO is ready, and `iso prune`
removes what a failed or deleted create left behind.

## Release cache

```bash
vsno cache add 4.20.46            # start mirroring a release, and return
vsno cache add 4.20.46 --watch    # ...or follow it to the end
vsno cache list -w                # versions, size, last used, and a live mirror's progress
vsno cache prune --dry-run        # unused releases
vsno cache prune --keep 1 --yes   # ...removing all but the newest unused one
vsno cache gc                     # free the space of images already deleted
```

The environment's registry holds mirrored OpenShift releases under `ocp/release/<digest>`. `create`
mirrors a version the first time it needs it unless `--no-cache`; `cache add` does it ahead of time.

`cache add` starts the mirror in a Job and returns, so one shell can queue several releases (they
take the builder's lock in turn) and `cache list` shows each one's progress, or `--watch` follows it
in place. Mirrors report their progress from the registry volume's growth since they began, which is
why a second process can see it.
The vSNO's install-config gets `imageDigestSources` pointing at the registry's **ClusterIP** (the
guest's own `cluster.local` hides the host's Service names) and the registry's CA as
`additionalTrustBundle`, so the installer and the cluster pull from the cluster.

Both the release image and its payload are mapped to the mirror, and the sigstore signature
(`sha256-<digest>.sig`) of the release image and of each of its component images is copied next to
them: the guest's image policy requires these signatures (from 4.22, for the component images too)
and looks for them in the registry it pulls from. Each use checks they're there, listing the
mirror's tags once, so a release mirrored before they were copied gets them on its next create.

Nightlies work the same way; the mirror step needs `registry.ci.openshift.org` credentials in the
cluster pull secret, which `create` already requires for nightlies. They have no signature to copy.

A release's size in `cache list` is every distinct blob its images reference, layers its neighbours
share included, so it is what the release holds rather than what pruning it alone would free.

`cache prune` deletes a release's images and then runs `registry garbage-collect` in a Job over the
registry's volume. The registry is unavailable for the seconds to minutes that takes, and that is
what actually frees the space; `--no-gc` skips it, and `vsno cache gc` runs it on its own (after
`vsno seed delete`, for instance). A release a vSNO still runs is refused, and so is a collection
while a mirror is pushing.

The registry's volume is 100 GiB by default (`vsno setup --registry-size`); it only ever grows.
