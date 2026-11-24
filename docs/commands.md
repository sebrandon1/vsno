# Commands

Every `vsno` command and its flags, as `vsno <command> --help` prints them. The guides
([getting started](getting-started.md), [vSNOs](vsnos.md), [upgrades](upgrades.md),
[performance](performance.md)) show these commands in use; this page is the reference.

## Global flags

Accepted by every command, before or after the command name:

| Flag | Meaning |
|---|---|
| `--kubeconfig <path>` | Path to the host cluster kubeconfig (default: `$KUBECONFIG`, then `~/.kube/config`) |
| `--context <name>` | Kubeconfig context to use |
| `-n, --namespace <name>` | Namespace that holds the vSNO environment (default `vsno`) |
| `-o, --output table\|wide\|json` | Output format (default `table`). `wide` is only meaningful for `list` and `iso list`; the other commands refuse it. `oc`, `ssh`, `kubeconfig`, `screenshot` and `iso eject` hand over a stream or only report that they are done, so they refuse `json` rather than ignore it. `start` and `stop` print `vsno`, `action` and, with `--wait`, `state`; `version` prints `version` |
| `--no-color` | Don't color the output (also `NO_COLOR=1`); never used with `-o json` |
| `-v, --verbose` | Trace what vsno does underneath on stderr: each cluster request as `oc`/`kubectl` would name it, and each command it runs in the builder pod. A request it repeats is shown once, then again with its count (`(x3)`) when the run ends. stdout stays clean, so `-o json` still pipes |
| `-h, --help` | Help for the command |
| `--version` | Print the vsno version (root only; also `vsno version`). Not `-v`, which is verbose |

Anything that deletes asks first, unless it takes `--yes`. The answer is `y`, or the name of what
you are destroying when the thing is hard to get back: a vSNO's disk, a namespace, a vSNO or a seed
image. The words older versions asked for (`prune`, `gc`, `yes`) still work.

Anything after a vSNO's name goes to `oc` or `ssh`, not to vsno. Put vsno's own flags, such as
`-n`, before the name: `vsno oc seed get nodes -o wide` sends `get nodes -o wide` to `oc`. The same
goes for `-v`: `vsno -v oc seed get nodes` traces the session, `vsno oc seed get nodes -v` passes
`-v` to `oc`.

With `--verbose`, a `create` reads like this:

```
→ get pods -n vsno -l app=builder
→ create virtualmachines.kubevirt.io target -n vsno
→ get datavolumes.cdi.kubevirt.io target -n vsno
→ exec builder: oc adm release mirror --registry-config "$dir/auth.json" --from "quay.io/…"
→ get virtualmachineinstances.kubevirt.io target -n vsno (x54)
```

## Contents

| Command | What it does |
|---|---|
| [`precheck`](#vsno-precheck) | Check whether the cluster can host vSNOs, and suggest settings |
| [`setup`](#vsno-setup) | Install what vSNOs need on the host cluster |
| [`status`](#vsno-status) | Show the state of an environment |
| [`create`](#vsno-create) | Create a vSNO |
| [`list`](#vsno-list) | List the vSNOs and their state |
| [`start`](#vsno-start) / [`stop`](#vsno-stop) | Start or stop a vSNO, keeping its disk |
| [`delete`](#vsno-delete) | Delete vSNOs |
| [`oc`](#vsno-oc) / [`ssh`](#vsno-ssh) / [`kubeconfig`](#vsno-kubeconfig) / [`screenshot`](#vsno-screenshot) | Reach a vSNO |
| [`prepare`](#vsno-prepare) | Get vSNOs ready for an Image Based Upgrade |
| [`seed`](#vsno-seed) | Make and manage seed images |
| [`upgrade`](#vsno-upgrade) | Upgrade a vSNO with an Image Based Upgrade |
| [`run`](#vsno-run) | Do a whole Image Based Upgrade from only a kubeconfig |
| [`snapshot`](#vsno-snapshot) | Keep a vSNO's disk, and restore from it |
| [`cache`](#vsno-cache) | Manage the mirrored OpenShift releases |
| [`iso`](#vsno-iso) | Show, delete and prune what creates leave behind |
| [`teardown`](#vsno-teardown) | Delete an environment |
| [`version`](#vsno-version) | Print the vsno version |

---

## `vsno precheck`

Inspects the host cluster and reports whether it can run two vSNO VMs for an Image Based Upgrade.
Exits non-zero if any check fails. The hardware probe runs a short-lived privileged pod on each
candidate worker in a temporary namespace (like `oc debug node`) and deletes it; nothing else is
changed.

| Flag | Meaning |
|---|---|
| `--node <name>` | Only probe this worker node |
| `--no-probe` | Skip the on-node hardware probe |
| `--probe-image <image>` | Image for the probe pod, needs `bash` + `chroot` (default `quay.io/centos/centos:stream9`) |
| `--probe-timeout <duration>` | How long to wait for each probe pod (default `3m`) |
| `--write-config <file>` | Write the suggested settings to this YAML file |
| `--seed-repo <repo>` | Registry repo for the seed image (e.g. `quay.io/me/ibu-seed`); checks local push credentials |
| `--vms <n>` | VMs that must run at the same time, seed + target (default `2`) |
| `--vm-cores <n>` | vCPUs per VM (default `16`) |
| `--vm-memory <n>` | Guest memory per VM, GiB (default `32`: sized for seed generation and the IBU precache. `vsno create` makes 16 GiB VMs by default, which the capacity line in `vsno list` uses) |
| `--min-disk <gb>` | Minimum size of the LVMS disk (default `600`) |

## `vsno setup`

Installs OpenShift Virtualization and LVM Storage (reused if already present), an `LVMCluster` on
the chosen disk unless `lvms-vg-vsno` already serves the node, and the environment namespace with
DNS, a builder, S3 and a TLS registry. Runs `precheck` first. Settings come from flags, then
`--config`, then precheck's suggestion. Without `-n` it sets up the environment that already exists;
name a new one with `-n`.

| Flag | Meaning |
|---|---|
| `--config <file>` | Settings file written by `vsno precheck --write-config` |
| `--node <name>` | Worker that will run the VMs (default: precheck's suggestion) |
| `--device <path>` | Disk for LVMS, a `/dev/disk/by-id` path (default: precheck's suggestion). It was `--disk`, which still works and warns |
| `--registry-size <size>` | Size of the in-cluster registry's volume, which holds mirrored releases; it only grows (default `100Gi`) |
| `--memory-overcommit <percent>` | OpenShift Virtualization's memory overcommit, e.g. `150`; changes every VM on the cluster, asks unless `--yes` |
| `--ksm` | Turn on kernel samepage merging for the vSNO node; asks unless `--yes` |
| `--operator-channel <cnv\|lvms>=<channel>` | Move an operator to a channel without asking; repeatable |
| `--upgrade-operators` | Upgrade operators that have an update available without asking |
| `--yes` | Approve taking over the LVMS disk without asking |
| `--force` | Take over an existing namespace vsno didn't create without asking, overwriting objects in it |
| `--no-probe` | Skip the precheck hardware probe (then `--node` is required) |
| `--dry-run` | List what setup would do and change nothing |
| `--manifests` | With `--dry-run`, also print the manifests, Secret values redacted; implies `--dry-run` |
| `--timeout <duration>` | How long to wait for each component (default `20m`) |

## `vsno status`

Reports, without changing anything: the host components, the support services and registry
endpoint, the vSNOs with their state, other VMs in the namespace, and the most recent stages in
`<namespace>/vsno-state`. Exits non-zero when the namespace or a component is missing or failing.

It ends with one `Next:` line saying what the environment is ready for — `vsno setup` when something
is not ready, `vsno create` with no vSNOs, the seed commands with one ready vSNO, and
`vsno upgrade <target> --seed <seed>` when two ready vSNOs are at different versions. `-o json` has
the same line as `next`.

| Flag | Meaning |
|---|---|
| `-w, --watch` | Refresh until interrupted |
| `--interval <duration>` | Refresh interval for `--watch` (default `15s`) |

`-o json` is the machine-readable report, and cannot be combined with `--watch`: a watch would print
one document after another.

## `vsno create`

Installs a single-node OpenShift cluster in a VM (`vsno setup` must have run): resolves `--version`
to a release image, builds the agent ISO in the builder pod, creates the VM, its disks, a Service
and DNS entries. The install takes about 45 minutes; `--watch` follows it. Without a name it picks
a free random one such as `brave-otter`.

| Flag | Meaning |
|---|---|
| `--version <version>` | OpenShift version to install (required unless `--from-snapshot`) |
| `--from-snapshot` | Restore the disk from the vSNO's snapshot instead of installing |
| `--node <name>` | Worker to run on (default: the node `setup` chose) |
| `--cores <n>` | vCPUs (default `16`) |
| `--memory <gib>` | Memory, GiB (default `16`) |
| `--disk <gib>` | Root disk, GiB (default `200`) |
| `--no-cache` | Install from the internet instead of the environment registry's mirror (the vSNO still trusts the registry's CA) |
| `-w, --watch` | Follow the install until the vSNO is ready |
| `--timeout <duration>` | How long to wait for the install (default `1h30m`) |

`--version` accepts a release (`4.20.46`), a minor (`4.20`, its latest z-stream), `4.22-nightly`, a
nightly's name, or a release image pullspec. Nightlies need `registry.ci.openshift.org` credentials
in the cluster pull secret.

## `vsno list`

Shows each vSNO's state (`building-iso`, `importing-iso`, `starting`, `installing`, `finishing`,
`making-seed`, `ready`, `stopped`, `failed`, `abandoned` — a Service with no VM behind it, older than
any build, which `vsno delete` cleans up), version, size, age and API, plus a footer with the memory left on the
node and how many more default-size vSNOs fit, and one `Next:` line with what they are ready for, as
`vsno status` reports it. Alias: `ls`.

| Flag | Meaning |
|---|---|
| `-w, --watch` | Refresh until interrupted |
| `--interval <duration>` | Refresh interval for `--watch` (default `15s`) |

## `vsno start`

Starts a stopped vSNO, keeping its disk.

| Flag | Meaning |
|---|---|
| `--wait` | Wait until the vSNO is ready |
| `--timeout <duration>` | How long to wait for the change (default `30m`) |

## `vsno stop`

Stops a vSNO, keeping its disk and freeing its memory.

| Flag | Meaning |
|---|---|
| `--wait` | Wait until the vSNO is stopped |
| `--timeout <duration>` | How long to wait for the change (default `30m`) |

## `vsno delete`

Removes each named vSNO: its VM and root disk, the ISO, its Service, its DNS entry and the
builder's files for it. Lists what it will delete and asks you to type the name(s) unless `--yes`.

| Flag | Meaning |
|---|---|
| `--dry-run` | List what would be deleted and change nothing |
| `--yes` | Don't ask for confirmation |
| `--timeout <duration>` | How long to wait for each vSNO to be gone (default `10m`) |

## `vsno oc`

Runs `oc` with the given arguments against the vSNO, as `kubeadmin`, from the builder pod. Flags
after the name go to `oc`.

```bash
vsno oc seed get nodes -o wide
vsno oc seed -- get clusterversion
```

Takes only the global flags.

## `vsno ssh`

Opens a shell on the vSNO as `core` (or runs the command), from the builder pod with the
environment's SSH key.

```bash
vsno ssh seed
vsno ssh seed -- uptime
```

Takes only the global flags.

## `vsno kubeconfig`

Prints the vSNO's admin kubeconfig. Its server, `https://api.<name>.vsno.lab:6443`, resolves only
inside the environment, so use it from a pod there, or use `vsno oc` from anywhere.

Takes only the global flags.

## `vsno screenshot`

Saves the vSNO's VGA console as a PNG: useful when the guest is unreachable, for example stuck in
emergency mode after a reboot.

| Flag | Meaning |
|---|---|
| `-f, --file <path>` | Output file (default `<name>.png`) |

## `vsno prepare`

Installs what an Image Based Upgrade needs on each vSNO, reusing what's already there: the
Lifecycle Agent and OADP from the vSNO's own catalog, and trust in the environment's registry. These
are the same steps `vsno seed precheck --install` takes, so a vSNO readied by either ends up the
same. LCA's own prerequisites have OADP on the seed as well as the target, so both roles get it.

A `target` also gets an S3 backup location and the seed pull secret. `vsno upgrade` prepares its
target itself, so this is for checking a vSNO ahead of time.

| Flag | Meaning |
|---|---|
| `--role seed\|target` | What the vSNOs are for (default `target`) |
| `--lca-ref <ref>` | Build LCA from this branch, tag or SHA in the cluster and deploy it |
| `--lca-repo <url>` | The git repository `--lca-ref` is in (default `https://github.com/openshift-kni/lifecycle-agent`); forks work |
| `--lca-image <image>` | Deploy this prebuilt LCA image instead of building one (default `main`) |

## `vsno seed`

Makes Image Based Upgrade seed images from vSNOs and manages them in the environment's registry,
as `seeds/<vSNO>:<version>`.

| Subcommand | What it does |
|---|---|
| [`seed create`](#vsno-seed-create) | Make a seed image from a vSNO |
| [`seed list`](#vsno-seed-list) | List the seed images in the environment's registry |
| [`seed delete`](#vsno-seed-delete) | Delete seed images |
| [`seed precheck`](#vsno-seed-precheck) | Check what a seed generation needs |
| [`seed sync`](#vsno-seed-sync) | Copy a seed image to another registry, or bring one in |

### `vsno seed create`

Turns a ready vSNO into a seed image: waits for the cluster to settle, installs the Lifecycle Agent
from the vSNO's own catalog if missing, makes the vSNO trust the registry, then runs LCA's seed
generation and follows it. The image lands in the registry as `seeds/<vsno>:<version>`. Takes about
15–30 minutes. A generation already running on the vSNO is refused before that, in a second.
Interrupting `create` does not stop LCA's generation: the vSNO reads `making-seed` until it ends.

| Flag | Meaning |
|---|---|
| `--image <ref>` | Push the seed here instead of the environment's registry |
| `--stop` | Stop the vSNO afterwards, to free its memory |
| `--timeout <duration>` | How long to wait for the seed generation (default `1h`) |

### `vsno seed list`

Lists the seed images in the environment's registry. Alias: `ls`. `AGE` is how long the seed has
been here: a copy made by `seed sync --from` keeps the upstream image's build time, so the time it
arrived is recorded in the environment's state and shown instead, marked `(synced)`; `-o json` has
both as `syncedAt` and `created`. Takes only the global flags.

### `vsno seed delete`

Removes seed images by full reference, `<vsno>:<version>`, or all of a vSNO's seeds by its name.
Lists them and asks you to type the arguments unless `--yes`.

| Flag | Meaning |
|---|---|
| `--dry-run` | List what would be deleted and change nothing |
| `--yes` | Don't ask for confirmation |

### `vsno seed sync`

Copies a seed image between the environment's registry and another one. `--to` pushes a seed out,
and the repository you name gets the seed's own tag, so `--to quay.io/me/seeds` puts
`seeds/<vsno>:<version>` there under `<version>`. `--from` brings an image in, stored as
`<vsno>:<version>`, so the version has to come with it. Exactly one of the two is required.

```bash
vsno seed sync seed-a:4.20.46 --to quay.io/me/seeds
vsno seed sync seed-a:4.20.46 --from registry.example.com/seeds/seed-a:4.20.46
```

Credentials for the registry that is not the environment's come from your container auth file (the
one `podman login` and `docker login` write), the first one that covers it, or `--authfile`; all
three flags fall back to `vsno config` (`seed.sync.to`, `seed.sync.authfile`, `seed.sync.ca`) when
they are not given;
nothing is written to the cluster. A peer registry on a private CA also needs `--ca`. An existing
tag on the far side is left alone unless `--overwrite`. The copy runs in the builder pod, so the
image never passes through your machine.

A pull is recorded in the environment's state, so `vsno seed list` shows when the seed arrived rather
than how old the upstream image is.

| Flag | Meaning |
|---|---|
| `--to <repo>` | Copy the seed to this repository, tagged with the seed's own tag |
| `--from <image>` | Copy the seed from this image |
| `--authfile <path>` | Credentials for the other registry (default: your container auth file) |
| `--ca <path>` | The other registry's CA certificate, when it is not publicly trusted |
| `--insecure` | The other registry may be plain HTTP or hold a certificate this environment does not trust |
| `--overwrite` | Replace the other registry's tag when it already exists |
| `-o json` | `name`, `version`, `source`, `destination`, `digest`, `sizeBytes` |

### `vsno seed precheck`

Reports everything making a seed image needs before the 15–30 minute generation: the vSNO's state,
the registry and its trust, the Lifecycle Agent, the image and the credentials to push it, and the
registry volume's free space (the generation pushes about 1.5 GiB, so under 5 GiB free is a warning
and under 2 GiB a failure). Only reads, and exits non-zero if anything is missing.

| Flag | Meaning |
|---|---|
| `--image <ref>` | The seed image reference to check, instead of the environment's registry |
| `--install` | Install LCA and OADP, and set up registry trust where they're missing |
| `--timeout <duration>` | How long `--install` may wait for each install (default `1h`) |

## `vsno upgrade`

Runs an Image Based Upgrade on the target with the Lifecycle Agent. Prepares the target, records
its identity, then runs Prep and Upgrade. Run again with the same seed, it carries on from where
the target is.

| Flag | Meaning |
|---|---|
| `--seed <vsno\|image>` | The seed: an image, `<vsno>:<version>`, or a vSNO for its newest seed (required unless `--abort`) |
| `--seed-version <version>` | The seed's OpenShift version, for an image outside the environment's registry |
| `--to prep\|upgrade` | Stop after this stage (default `upgrade`) |
| `--finalize` | Finalize the upgrade (stage Idle), dropping the old version |
| `--rollback` | Roll the upgrade back to the old version |
| `--abort` | Cancel the target's Prep, moving it back to Idle (no `--seed`) |
| `--lca-ref <ref>` | Build LCA from this branch, tag or SHA and deploy it |
| `--lca-repo <url>` | The git repository `--lca-ref` is in (default `https://github.com/openshift-kni/lifecycle-agent`) |
| `--lca-image <image>` | Deploy this prebuilt LCA image instead of building one (default `main`) |

## `vsno run`

Does an Image Based Upgrade end to end: `setup`, then `vsnos` (a seed and a target, both started
before waiting for either), then `seed`, then `upgrade`. The plan is shown and confirmed first,
unless `--yes`. Each stage reuses what's there, so running it again after a stop carries on.

| Flag | Meaning |
|---|---|
| `--target-version <version>` | The target's OpenShift version, or its release pullspec (default: the release before the seed's) |
| `--seed-version <version>` | The seed's OpenShift version, or its release pullspec (default: the newest stable release of the host's minor) |
| `--target-name <name>` | The target vSNO's name (default `target`) |
| `--seed-name <name>` | The seed vSNO's name (default `seed`) |
| `--seed-image <ref>` | Upgrade to this existing seed image instead of making one |
| `--seed-repo <repo>` | Push the seed image to `<repo>:<version>` instead of the environment's registry |
| `--node <name>` | Worker for the vSNOs (default: setup's) |
| `--timeout <duration>` | How long the setup stage may take (default `20m`) |
| `--cores <n>` | vCPUs per vSNO (default as `vsno create`) |
| `--memory <gib>` | GiB of memory per vSNO (default as `vsno create`) |
| `--device <path>` | Disk for LVM Storage on the node, as `vsno setup --device` (default: precheck's pick). It was `--disk`, which still works and warns |
| `--lca-ref <ref>` | Build LCA from this ref and use it on both vSNOs |
| `--lca-repo <url>` | The git repository `--lca-ref` is in |
| `--lca-image <image>` | Use this prebuilt LCA image on both vSNOs |
| `--finalize` | Finalize the upgrade afterwards |
| `--rollback` | Roll the upgrade back afterwards |
| `--delete` | Delete both vSNOs afterwards |
| `--restart` | Run every stage again instead of skipping the ones an earlier run of this plan finished |
| `-f, --file <file>` | Settings file (YAML) with the same settings as the flags |
| `--yes` | Run the plan without asking (for CI) |

## `vsno snapshot`

Keeps a vSNO's installed root disk as a `VolumeSnapshot`; `vsno create <name> --from-snapshot`
restores it in minutes instead of reinstalling. A snapshot is only restored under the same vSNO
name. A running vSNO is stopped for the snapshot and started again afterwards. Alias: `snapshots`.

| Subcommand | What it does |
|---|---|
| [`snapshot create`](#vsno-snapshot-create) | Snapshot a vSNO's disk |
| [`snapshot list`](#vsno-snapshot-list) | List the snapshots |
| [`snapshot delete`](#vsno-snapshot-delete) | Delete snapshots |

### `vsno snapshot create`

Stops the vSNO if needed, snapshots its disk, and starts it again.

| Flag | Meaning |
|---|---|
| `--timeout <duration>` | How long to wait for the storage driver (default `30m`) |

### `vsno snapshot list`

Lists the kept snapshots and whether their vSNO still exists. Alias: `ls`.

### `vsno snapshot delete`

Removes the named snapshots and the installer files kept with them. Lists them and asks you to type
`yes` unless `--yes`.

| Flag | Meaning |
|---|---|
| `--dry-run` | List what would be deleted and change nothing |
| `--yes` | Don't ask for confirmation |

## `vsno cache`

Lists the OpenShift releases mirrored into the environment's registry, which vSNOs install from
instead of pulling 15–20 GB from the internet. `create` mirrors a release the first time it is used;
`--no-cache` turns that off.

| Subcommand | What it does |
|---|---|
| [`cache add`](#vsno-cache-add) | Mirror an OpenShift release into the environment's registry |
| [`cache list`](#vsno-cache-list) | List the mirrored releases, their size and the vSNOs using them |
| [`cache prune`](#vsno-cache-prune) | Remove cached releases, and collect the registry's garbage |
| [`cache gc`](#vsno-cache-gc) | Free the space of deleted images by collecting the registry's garbage |

### `vsno cache add`

Mirrors a release's payload into the environment's registry ahead of time. `<version>` takes what
`create` accepts.

It returns as soon as the mirror is running: the work happens in a Job in the environment, so the
mirror carries on after the command exits — and after the shell closes. `--watch` follows it here
until it finishes instead. `vsno cache list` shows a running mirror's progress from any shell: how
much it has pushed, how fast, and how much is left.

A release already cached comes back at once, and so does one whose mirror is still running: that
second call follows the existing Job rather than starting another. Mirrors queue on the builder's
lock, so `cache add` several releases and walk away; they run one at a time.

A mirror that stopped part way keeps what it already pushed, so running the same command again
carries on from there rather than starting over. A failed mirror leaves its Job and log in place
(`kubectl -n vsno logs job/cache-mirror-<first 16 hex digits of the payload digest>`); a finished one removes them.

| Flag | Meaning |
|---|---|
| `-w`, `--watch` | Follow the mirror until it finishes, exiting non-zero if it fails |

### `vsno cache list`

Lists the mirrored releases, their size, and the vSNOs using them. A release being mirrored shows
its progress — bytes pushed against the estimate, the rate, and what is left. A mirror that
finished while nothing was watching is recorded before the list is shown. Alias: `ls`.

| Flag | Meaning |
|---|---|
| `-w`, `--watch` | Refresh until interrupted |
| `--interval <duration>` | Refresh interval for `--watch` (default 15s) |

### `vsno cache prune`

Removes a cached release, or the ones no vSNO uses (`--keep N` keeps the N most recently used). It
deletes the images and then runs the registry's garbage collector, which is what frees the space.
Lists what it would remove and asks you to type `prune` unless `--yes`. A release a vSNO still runs
is refused.

| Flag | Meaning |
|---|---|
| `--keep <n>` | Keep the N most recently used unused releases |
| `--no-gc` | Skip the registry garbage collection, which is what frees the space |
| `--dry-run` | List what would be removed and change nothing |
| `--yes` | Don't ask for confirmation |

### `vsno cache gc`

Runs the registry's own garbage collector over its volume. The registry is unavailable while it
runs; it refuses while a release mirror is running.

| Flag | Meaning |
|---|---|
| `--yes` | Don't ask for confirmation |
| `--dry-run` | Report what the collection would free — the volume's used bytes less what the tagged releases and seed images take — and change nothing |

## `vsno iso`

Lists what the environment holds beyond its vSNOs: the ISOs the builder serves, the installer's
build directories, the cached `openshift-install` binaries (one per release) and the ISO
DataVolumes, with what still uses each. Ends with the builder volume's usage. `vsno iso` alone is
`vsno iso list`. Alias: `isos`.

| Subcommand | What it does |
|---|---|
| [`iso list`](#vsno-iso-list) | List the ISOs, installer caches and ISO DataVolumes, and what uses them |
| [`iso info`](#vsno-iso-info) | Show what one vSNO, installer or DataVolume holds, and who uses it |
| [`iso delete`](#vsno-iso-delete) | Delete the ISOs, installers or ISO DataVolumes you name |
| [`iso eject`](#vsno-iso-eject) | Remove a vSNO's install ISO, which it no longer boots from |
| [`iso prune`](#vsno-iso-prune) | Remove the ISOs, installers and ISO DataVolumes no vSNO uses |

### `vsno iso list`

Lists the ISOs, installer caches and ISO DataVolumes, and what uses them, with each one's age.
Alias: `ls`.

### `vsno iso info`

Shows one name's builder files — path, version, size, age and who uses them — its ISO DataVolume,
the release an installer came from, and the builder volume's usage. A vSNO name brings its served
ISO, build directory and DataVolume; an installer or DataVolume name brings itself.

Takes one name and the global flags.

### `vsno iso delete`

Removes what you name: a vSNO's served ISO and build directory, a cached `openshift-install`
binary, or an ISO DataVolume. Anything a vSNO still uses is refused. Asks "are you sure?" unless
`--yes`.

| Flag | Meaning |
|---|---|
| `--dry-run` | List what would be removed and change nothing |
| `--yes` | Don't ask for confirmation |

### `vsno iso eject`

Removes the install ISO from a vSNO that has finished installing: the CD-ROM and its volume from
the VM, the ISO DataVolume, and the builder's copy.

Takes only the global flags.

### `vsno iso prune`

Removes what a finished or failed `create` left behind: the served ISO and build directory of
vSNOs that no longer exist, the `openshift-install` binaries no vSNO runs (`--keep N` keeps the N
newest), and their ISO DataVolumes. Lists them and asks you to type `prune` unless `--yes`.

| Flag | Meaning |
|---|---|
| `--keep <n>` | Keep the N newest unused installers |
| `--dry-run` | List what would be removed and change nothing |
| `--yes` | Don't ask for confirmation |

## `vsno config`

Reads and writes the settings that do not change between runs, so they need not be retyped as
flags. They live in `$XDG_CONFIG_HOME/vsno/config` (`~/.config/vsno/config`), one `KEY=VALUE` per
line, mode `0600`. Nothing secret is stored there: a setting names the path to a credentials file,
never its contents.

A flag beats the environment, which beats the file:

| Setting | Flag | Environment |
|---|---|---|
| `namespace` | `-n` | `VSNO_NAMESPACE` |
| `output` | `-o` | `VSNO_OUTPUT` |
| `seed.sync.to` | `--to` | `VSNO_SEED_SYNC_TO` |
| `seed.sync.authfile` | `--authfile` | `VSNO_SEED_SYNC_AUTHFILE` |
| `seed.sync.ca` | `--ca` | `VSNO_SEED_SYNC_CA` |

A configured `namespace` is treated as chosen: `vsno list` then lists that environment rather than
all of them, and if it is not a vsno environment the commands say so and name the key to unset. A
configured `seed.sync.to` applies only when neither `--to` nor `--from` is given, so a pull never
inherits a destination.

| Subcommand | What it does |
|---|---|
| [`config`](#vsno-config) | List the settings, their values and where each came from |
| [`config path`](#vsno-config) | Print the file's path |
| `config get <key>` | Print one setting; exits non-zero when it is not set |
| `config set <key> <value>` | Set one setting, keeping the file's comments and order |
| `config unset <key>` | Forget one setting |

```bash
vsno config set seed.sync.to quay.io/me/seeds
vsno seed sync seed-a:4.22.22
```

## `vsno doctor`

Looks for the states that read as a hang or a boot failure but are something vsno knows about: an
ImageBasedUpgrade whose `Idle` condition is stuck at a reason LCA's controller does not act on
([#125](https://github.com/sebrandon1/vsno/issues/125)); a seed generation LCA is running that no
vsno process is following ([#127](https://github.com/sebrandon1/vsno/issues/127)); a vSNO whose
`kubelet` and `crio` a half-finished seed generation left disabled ([#129](https://github.com/sebrandon1/vsno/issues/129));
a `Released` PersistentVolume still holding a disk; and a registry too full for the next seed image.

It only reads unless `--fix` is given, and `--fix` runs only two reversible actions — enabling crio
and kubelet on such a vSNO, and the status patch that unsticks LCA's Idle condition. It never
deletes anything, and it prints nothing on a healthy environment.

| Flag | Meaning |
|---|---|
| `--fix` | Apply the two reversible fixes; everything else is reported |

## `vsno report`

Reads the environment once and writes everything a bug report or a demo wants as one document: the
status (with its `next` line), the vSNOs, the release cache and the registry volume, the seed images,
the builder's leftovers, and a `warnings` list for whatever could not be read. JSON unless
`--markdown`. Nothing it writes carries credentials: registry logins, S3 keys and the pull secret are
named, never quoted.

```bash
vsno report > vsno-report.json
vsno report --markdown > vsno-report.md
```

| Flag | Meaning |
|---|---|
| `--markdown` | Write Markdown tables instead of JSON |

## `vsno teardown`

Deletes the environment namespace: its VMs, support services, volumes and generated credentials.
Only deletes a namespace vsno created. With `--host` it also removes what `setup` installed on the
host. Lists what it will delete and asks you to type the namespace (or `host` with `--host`) unless
`--yes`. Without `-n` it tears down the environment that exists, and refuses to choose when there
are several.

| Flag | Meaning |
|---|---|
| `--host` | Also remove HyperConverged, the LVMCluster and both operators |
| `--force` | Ignore the safety checks (VMs or volumes elsewhere, a namespace vsno didn't create) |
| `--dry-run` | List what would be deleted and change nothing |
| `--yes` | Don't ask for confirmation |
| `--timeout <duration>` | How long to wait for each deletion, and for the volumes to be released (default `10m`) |

A namespace-only teardown deletes the namespace but not the volumes behind its claims: LVMS keeps a
PV `Released` until its `LogicalVolume` is gone, which it can't be while the device is still mounted.
Teardown therefore re-reads the PersistentVolumes afterwards and fails, naming the volume, its size
and the host-side command that finds the holder, rather than reporting success while the disk is
still allocated. `-o json` lists them under `allocated`.

## `vsno version`

Prints the vsno version. Takes only the global flags.

---

Back to the [README](../README.md) · [How-To guide](howto.md)
