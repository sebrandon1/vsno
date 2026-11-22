# vsno

Create, list and delete **virtual single-node OpenShift clusters (vSNOs)** quickly, as many as
you need. The vSNOs run as OpenShift Virtualization VMs on a bare-metal worker of an OpenShift
cluster you already have, so you don't need a spare hypervisor or any reprovisioning.

One workflow built on top of that is testing
[lifecycle-agent](https://github.com/openshift-kni/lifecycle-agent) **Image Based Upgrades
(IBU)** between two vSNOs.

> **Status:** milestones 1–7 are done: `precheck`, `setup`, `status`, `teardown`, the vSNO
> lifecycle (`create`, `list`, `delete`, `start`, `stop`, `oc`, `ssh`, `kubeconfig`, `screenshot`),
> seed images, fast creates from an in-cluster release cache, Image Based Upgrades (`prepare`,
> `upgrade`), testing lifecycle-agent branches in them, and `vsno run` for the whole flow.
>
> See the [roadmap issues](https://github.com/sebrandon1/vsno/issues?q=label%3Aroadmap).

## Install

Requires Go 1.27 or newer.

Download a binary for Linux or macOS (amd64 or arm64) from the
[releases](https://github.com/sebrandon1/vsno/releases), with `checksums.sha256` to verify it, or
build it:

```bash
go install github.com/sebrandon1/vsno@latest
# or
make build && ./vsno --help
```

`vsno version` (or `vsno --version`) prints the build version.

Global flags, accepted by every command:

| Flag | Meaning |
|---|---|
| `--kubeconfig <path>` | Host cluster kubeconfig (default: `$KUBECONFIG`, then `~/.kube/config`) |
| `--context <name>` | Kubeconfig context to use |
| `-n, --namespace <name>` | Namespace that holds the vSNO environment (default `vsno`) |
| `-o, --output table\|json` | Output format (default `table`) |
| `--no-color` | Plain output. Color is used only on a terminal, and never with `NO_COLOR` set or `-o json` |

## precheck

All vsno needs is a kubeconfig for a cluster with a bare-metal worker. It uses `$KUBECONFIG`
(or `~/.kube/config`) like `oc` does, or you can pass `--kubeconfig`:

```bash
vsno precheck
# or
vsno precheck --kubeconfig ~/cluster/kubeconfig
```

Long-running steps show a spinner, elapsed time and a progress bar:

```text
✓ Checking cluster configuration (2s): 4.22.0
⠼ Probing 2 worker(s) [##########----------] 1/2 worker-1 ✓ (9s)
```

It checks:

| Area | What |
|---|---|
| Access | You are cluster-admin, and the cluster is OpenShift (its version is reported) |
| Networks | The guest networks (`10.0.2.0/24`, `10.132.0.0/14`, `172.31.0.0/16`) don't overlap the host's pod and service networks |
| Catalog | `kubevirt-hyperconverged` and `lvms-operator` are available. It picks the LVMS channel that matches the host's version. |
| Existing state | OpenShift Virtualization or LVMS already installed (and whether their channel offers a newer CSV: `up to date` or `update available`), an existing `lvms-vg-vsno` StorageClass, an existing environment namespace (`-n`, default `vsno`) |
| Hub | Whether the cluster is managed by an ACM hub (hub policies may revert operators vsno installs) |
| Registry | The seed image goes to an in-cluster registry by default, so nothing is needed. Only if you pass the optional `--seed-repo` (e.g. `quay.io/<you>/ibu-seed`) does it check your local podman/docker auth files for push credentials. |
| Each worker | `/dev/kvm`, bare metal or nested virtualization, CPU threads, schedulable memory, and an empty disk of at least 600 GB for LVMS |

The per-worker hardware probe runs a short-lived privileged pod on each worker in a temporary
`vsno-precheck-*` namespace, the same way `oc debug node` does, and deletes it afterwards.
Nothing else is changed. Use `--no-probe` to skip the probe.

Example output (shortened) for a 3-master, 2-worker cluster where `worker-1` is a bare-metal
server and `worker-0` is a small VM:

```text
Cluster
✓  Cluster access     cluster-admin
✓  OpenShift version  4.22.0
✓  Network overlap    host 10.128.0.0/14,172.30.0.0/16 vs guest 10.0.2.0/24,10.132.0.0/14,172.31.0.0/16
   ...

Operators, storage and registry
✓  Catalog: LVM Storage  lvms-operator channel stable-4.22
i  Seed registry         in-cluster registry (default); pass --seed-repo to push to an external registry instead
   ...

Workers
i  Worker nodes  2 candidate(s)
✓  worker-1  eligible, suggested
   ✓  virtualization  bare metal (Example Server), /dev/kvm present
   ✓  cpu             104 threads (need 32 for 2 VMs)
   ✓  memory          80 GiB schedulable (need 66 GiB)
   ✓  disk            sdb 1919GB empty: /dev/disk/by-id/wwn-0x5000000000000002
i  worker-0  not usable
   !  virtualization  node is itself a VM (virt=kvm) with nested KVM
                      → Works, but nested guests are much slower. Prefer a bare-metal worker.
   i  cpu             4 threads; one VM needs 16
   ...
✓  Eligible node  worker-1 (of 1 eligible)

Suggested settings for `vsno setup`:
  node          worker-1
  disk          /dev/disk/by-id/wwn-0x5000000000000002 (1919 GB)
  host DNS      172.30.0.10
  LVMS channel  stable-4.22
  CNV channel   stable
```

Useful flags:

| Flag | Meaning |
|---|---|
| `-o json` | Machine-readable report |
| `--write-config vsno.yaml` | Save the suggested settings to a file |
| `--seed-repo <repo>` | Optional: check push credentials for an external seed registry |
| `--node <name>` | Probe only that worker |
| `--no-probe` | Skip the per-worker hardware probe |
| `--probe-timeout <duration>` | How long to wait for each probe pod (default `3m`) |
| `--vms`, `--vm-cores`, `--vm-memory`, `--min-disk` | Change the sizing requirements |
| `--probe-image` | Use a different probe image (it needs `bash` and `chroot`) |

`precheck` exits non-zero if any check fails. When at least one worker is eligible, the
problems found on the other workers are shown as info rather than failures.

In `-o json`, each result has a `group` (`cluster`, `install` or `workers`), per-node results
have a `node`, and `nodes` lists each evaluated worker with `eligible`.

## setup

```bash
vsno precheck --write-config vsno.yaml   # review the suggested node and disk
vsno setup --config vsno.yaml
vsno setup --dry-run                     # list what setup would do, change nothing
```

`setup` installs what the vSNOs need on the host cluster:

1. OpenShift Virtualization and LVM Storage. Both are reused if their operator is already
   installed.
2. An `LVMCluster` on the chosen disk, which creates StorageClass `lvms-vg-vsno`. This is skipped
   when that StorageClass already serves the node. Taking over a disk erases it, so `setup` asks you
   to type the node name unless you pass `--yes`.
3. The environment namespace (`-n`, default `vsno`) with dnsmasq (guest DNS), a builder pod, S3 (SeaweedFS, bucket `ibu`)
   and a TLS registry. The S3 keys, the registry CA, its certificate and the login are generated
   once and stored as Secrets in the namespace.

It runs `precheck` first. Cluster-level failures stop it, while CPU or memory shortfalls on the
node are only warnings. Settings come from `--node`/`--disk`, then `--config`, then the
precheck's suggestion. If `--node` names a different node than `--config`, the config's disk is
dropped, since it belongs to the other node. A rerun reuses everything and reports each object as `unchanged`.
Progress and the result are recorded in ConfigMap `<namespace>/vsno-state`.

### Fitting more vSNOs: memory overcommit and KSM

```bash
vsno setup --memory-overcommit 150 --ksm
```

`--memory-overcommit <percent>` sets OpenShift Virtualization's
`higherWorkloadDensity.memoryOvercommitPercentage`: at 150, a 16 GiB vSNO's pod requests about
11 GiB. `--ksm` turns on kernel samepage merging for the vSNO node (`ksmConfiguration`; KSM that is already on is only widened, never narrowed), so
guests running the same OpenShift share identical memory pages. Both change every VM on the
cluster, so setup asks first unless you pass `--yes`, and `--dry-run` shows the change.

Setup records the settings it replaced in `vsno-state`, and a plain `vsno teardown` puts them back
before deleting the namespace (`teardown --host` deletes HyperConverged instead). `vsno create` and
`vsno list` count a node's free memory at the overcommit in effect, and `vsno status` and
`vsno precheck` show it. Overcommitted guests can run out of host memory under load; keep the
percentage modest.

Before installing anything, setup checks the two operators it manages. An operator is out of date
when its channel offers a newer CSV than the one it runs, when a `Manual` Subscription has an
InstallPlan waiting for approval, or when its Subscription is on an older channel than the one
precheck picks for this OpenShift version. Setup lists each one with the CSV it runs and the one it
would install, then asks `Upgrade <operator> from <installed> to <available>?` and upgrades the ones
you accept: it moves the Subscription to the channel, approves the InstallPlan and waits for the new
CSV to reach `Succeeded` (and, for OpenShift Virtualization, for HyperConverged to be `Available`
again). The step then reports `upgraded`. `--upgrade-operators` answers yes to all of them; `--yes`
only approves the disk. A CNV upgrade restarts OpenShift Virtualization, so the running VMs it would
affect are named first. Without a terminal setup warns and keeps the installed version, and a rerun
reports the operators as `reused`. `--operator-channel cnv=candidate` (repeatable, one per operator)
moves an operator to a channel you choose, without asking: setup patches the Subscription, approves
the InstallPlan and waits, and refuses a channel the catalog doesn't list or one whose CSV is older
than the installed one, since OLM does not downgrade.

Setup asks before taking over an existing namespace that vsno didn't create: one with neither the
`app.kubernetes.io/managed-by=vsno` label nor a `vsno-state` ConfigMap. It would overwrite objects
there, such as an environment built by hand, so it shows that warning and asks you to type the
namespace name to continue. Pass `--force` to take it over without asking, or use another
namespace with `-n`. If the namespace holds vSNOs, setup refuses without asking, since it would
fight the VMs already in it; only `--force` takes it over.

`--dry-run` lists each step (reuse what is installed, apply, or upgrade an operator) and each
object, with what applying it would do:
- `create`: it doesn't exist yet
- `reapply`: vsno applied it before; usually nothing changes
- `take over`: it exists but vsno didn't create it. Setup would overwrite it, so the plan ends with a
  warning. Run it before pointing setup at a namespace you made by hand.

Every object setup applies carries the label `app.kubernetes.io/managed-by=vsno`.

If no worker has room for the VMs, precheck can't suggest a node and you must pass `--node`. Only
memory used by the same namespace counts as free, so a second environment on a node that already
runs one (for example `-n vsno-dev` next to `vsno`) usually needs `--node`.

| Flag | Meaning |
|---|---|
| `--node`, `--disk` | Worker for the VMs and the `/dev/disk/by-id` path for LVMS |
| `--config <file>` | Settings written by `precheck --write-config` |
| `--yes` | Approve taking over the disk without asking |
| `--upgrade-operators` | Upgrade the operators that have an update available without asking |
| `--operator-channel <op>=<channel>` | Move an operator (`cnv` or `lvms`) to another channel without asking; repeatable. Refused when the catalog doesn't list the channel or it would install an older CSV |
| `--force` | Take over an existing namespace vsno didn't create without asking (see below) |
| `--dry-run` | List what setup would do and change nothing (see below) |
| `--manifests` | With `--dry-run`, also print the manifests, Secret values redacted (implies `--dry-run`) |
| `--no-probe` | Skip the hardware probe (then `--node` is required) |
| `--timeout <duration>` | How long to wait for each component (default `20m`) |
| `-n <namespace>` | Environment namespace (default `vsno`) |

## vSNOs

```bash
vsno create a --version 4.20 --watch       # latest 4.20 z-stream, follow the install
vsno create b --version 4.22-nightly       # latest accepted 4.22 nightly; returns once the VM exists
vsno create --version 4.20                 # no name: a random one, such as brave-otter
vsno list                                  # every environment; --watch, or -o json
vsno list -n vsno-dev                      # just one environment
vsno oc a get nodes                        # oc against the vSNO
vsno ssh a                                 # a shell as core; vsno ssh a -- uptime
vsno kubeconfig a > a.kubeconfig
vsno screenshot a                          # a.png of the console
vsno stop a && vsno start a --wait
vsno delete a b
```

`create` needs an environment from `vsno setup`. Without `-n`, the vSNO commands (and `status`) use
the environment `vsno setup` created, if there is exactly one; with several, pass `-n`. When the
default `vsno` is one of several, the command uses it and says so on stderr — `note: vsno is one of
several vsno environments (also: vsno-dev); pass -n to choose another` — so an output is never
taken for one of an environment it didn't describe.

`create`:
1. resolves `--version` to a release image
2. builds the agent ISO in the builder pod
3. creates the VM, its disks, a Service and DNS entries
4. returns. The install takes about 45 minutes: `--watch` follows it with a progress bar, or
   follow it later with `vsno list --watch`.

`--version` accepts:
- a release: `4.20.46`
- a minor: `4.20`, its latest stable z-stream
- `4.22-nightly`: the latest accepted nightly
- a nightly's name: `4.22.0-0.nightly-…`
- a release image pullspec

Nightlies need credentials for `registry.ci.openshift.org` in the cluster pull secret
(`openshift-config/pull-secret`). `create` checks for them first.

| Flag | Meaning |
|---|---|
| `--version` | OpenShift version (required) |
| `--cores`, `--memory`, `--disk` | Size: defaults 16 vCPU / 16 GiB / 200 GiB; minimum 8 / 16 / 120. Use `--memory 24` or `32` for heavier work such as IBU |
| `--node` | Worker to run on (default: the node `setup` chose) |
| `--no-cache` | Install from the internet instead of the registry's mirror |
| `--watch`, `-w` | Follow the install with progress until the vSNO is ready |
| `--timeout` | How long to wait for the install (default `90m`) |

`create` refuses a vSNO that doesn't fit next to what already runs on the node, and shows the free
memory. `vsno stop` frees a vSNO's memory and keeps its disk.

The first `create` of a version also mirrors that release's payload into the environment's
registry (15–20 GB, once), so every later vSNO of it installs from the cluster: `--no-cache` skips
that. A vSNO created with `--watch` records how long each phase took; `list -o wide` shows it.

`list` shows every vSNO in every vsno environment, with a NAMESPACE column: the namespaces vsno
labeled, plus any that hold the builder pod (an environment assembled by hand predates the label).
`-n` narrows it to one. Each row has the vSNO's state, version, size, age and API, and the footer
says how many more default-size vSNOs fit on each node. The states are `importing-iso`, `starting`,
`installing` (with the installer's progress), `finishing`, `making-seed`, `ready`, `stopped`,
`failed` and `deleting`.

Two states are progress that would otherwise read as a failure: `finishing` says "control plane
rolling out" while the ClusterVersion is not Available yet, and `making-seed` covers seed
generation, which stops the guest's services so nothing answers. The ClusterVersion's own message
(the CVO's `Unable to apply …: MultipleErrors` while a pod cannot schedule, say) is in `-o json`
(`cvMessage`) and in `vsno status`, and `list` shows it in DETAIL only once the rollout is more
than 20 minutes old — a real stuck install still shows its error.

Each vSNO's API is `https://api.<name>.vsno.lab:6443`. Only the environment resolves that name,
so `oc`, `ssh` and `kubeconfig` work through the builder pod. Everything after the vSNO's name goes
to `oc` or `ssh`, so put vsno's own flags, such as `-n`, before the name.

`delete` removes the VM, its disks, the ISO, its Service, its DNS entry and the builder's files for
it. It asks first unless you pass `--yes`, and `--dry-run` only lists what it would delete.

## Seed images

```bash
vsno seed precheck a               # is a ready? what would create wait on?
vsno seed precheck a --install     # ...and set up LCA and registry trust
vsno seed create a                 # make seeds/a:4.20.46 from vSNO a
vsno seed create a --stop          # ...and stop a afterwards, to free its memory
vsno seed list                     # or ls, or -o json
vsno seed delete a:4.20.46 --dry-run
vsno seed delete a                 # all of a's seeds
```

`seed precheck` reports everything `seed create` needs before it starts the 15–30 minute generation:
the vSNO is ready, the environment's registry is usable, the vSNO's API answers and its cluster is
settled, the Lifecycle Agent is installed (or the vSNO's catalog has it), the vSNO trusts the
registry's CA, and the seed image can be pushed. It only reads, and exits non-zero if anything is
missing. `--install` sets up the two things vsno can — subscribing to LCA from the vSNO's own catalog
and making the vSNO trust the registry — then reports again. `create` runs the same checks first, so
the two cannot drift.

`seed create` turns a ready vSNO into an Image Based Upgrade seed image. Seed generation has to run
on the vSNO itself, so the cluster is stopped while the Lifecycle Agent builds the image and comes
back when it's done. `create`:
1. waits for the cluster's operators to settle
2. installs the Lifecycle Agent from the vSNO's own catalog if it's missing
3. makes the vSNO trust the environment's registry
4. runs the seed generation, following it to the end

The image lands in the environment's registry as `seeds/<vsno>:<version>`. `--image` pushes elsewhere
(the host cluster's pull secret must have credentials for that registry). It takes about 15–30
minutes; the vSNO's API is down for most of it.

| Flag | Meaning |
|---|---|
| `--image` | Push the seed here instead of the environment's registry |
| `--stop` | Stop the vSNO afterwards |
| `--timeout` | How long to wait for the generation (default `1h`) |

When the install finishes, `create` ejects the ISO: the CD-ROM and its volume leave the VM, the
`<name>-iso` DataVolume is deleted, and the builder's copy goes. The vSNO boots from its root disk
from then on, which frees about 4 GB. `vsno iso eject <vsno>` does the same for a vSNO from an
older vsno.

`seed list` shows each seed image's reference, the vSNO it came from, its OpenShift version, size and
age. `seed delete` removes tags by full reference, `<vsno>:<version>` or `<vsno>`. It asks you to
type the arguments first unless `--yes`, and `--dry-run` only lists. The registry frees the disk
space only when it is garbage collected, so deletion does not shrink it immediately.

## Image Based Upgrade

```bash
vsno seed create seed-421                        # a seed image from a ready vSNO (see Seed images)
vsno upgrade target --seed seed-421              # Prep, then Upgrade, to the seed's version
vsno upgrade target --seed seed-421 --finalize   # ...and finalize, dropping the old version
vsno upgrade target --seed seed-421 --to prep    # stop after Prep
vsno prepare target                              # only get a vSNO ready, ahead of time
```

`upgrade` runs an Image Based Upgrade on the target with the Lifecycle Agent (LCA). `--seed` is a
seed image (`vsno seed list`), `<vsno>:<version>`, or a vSNO, meaning its newest seed. For an image
outside the environment's registry, pass its reference and `--seed-version`.

1. It prepares the target, like `vsno prepare`: LCA and OADP from the vSNO's own catalog, trust in
   the environment's registry, a backup location in the environment's S3 (reached by ClusterIP)
   that OADP reports Available, and the `seed-pull-secret` LCA pulls the seed with.
2. It records the target's identity: version, clusterID, infrastructureName, node and API URL. When
   the seed's vSNO answers, its LCA must be the same version as the target's.
3. **Prep** pulls the seed's images ahead, shown as a progress bar.
4. **Upgrade** reboots the node into the seed's version. The API is down for about 15 minutes; if
   LCA rolls the upgrade back by itself, upgrade says so and stops.
5. It prints the identity before and after, which must differ only in version, and how long each
   stage and phase took.

`--finalize` then moves to Idle, dropping the old version; `--rollback` goes back to it. Run again
with the same seed, upgrade carries on from where the target is, so an upgrade can be finalized or
rolled back later. A target part way through with another seed is refused. `vsno list` shows a
vSNO's IBU stage in its detail, such as `IBU Upgrade done`.

`prepare <vsno>... --role seed` sets up only what a seed needs (LCA, OADP, registry trust).

### Testing an LCA branch

```bash
vsno prepare seed-421 target --lca-ref my-fix --lca-repo https://github.com/me/lifecycle-agent
vsno seed create seed-421                                 # the seed is made with that LCA
vsno upgrade target --seed seed-421 --finalize            # and the upgrade runs with it
vsno prepare target --lca-image quay.io/me/lca:test       # a prebuilt image instead of a build
```

`--lca-ref` (on `prepare` and `upgrade`) builds LCA from a branch, tag or SHA without building
anything on your laptop. A privileged buildah pod in the environment namespace, next to the builder,
clones the repository at the ref and builds its `Dockerfile` for `linux/amd64`, using the host
cluster's pull secret for the base images. It pushes the result to the environment's registry as
`lca:<sha>`, and skips the build when that tag is already there. `--lca-repo` names a fork (default
`openshift-kni/lifecycle-agent`), and `--lca-image` deploys a prebuilt image instead of building one.

Each vSNO then runs that LCA instead of its catalog's:
- the OLM Subscription and CSV are removed; the CRDs and the ImageBasedUpgrade stay
- the ref's `config/default` manifests are applied with the controller image replaced, as
  `make deploy` does
- the registry's login is added to the vSNO's pull secret
- vsno waits for the controller to run the new image

It warns when the ref's `VERSION` isn't the vSNO's OpenShift minor. The source is recorded on the
vSNO and shown in the `LCA` column of `vsno list -o wide`. Later `prepare`, `upgrade` and
`seed create` runs keep an LCA deployed from a ref rather than replacing it with the catalog's.
Prepare seed and target from the same ref: `upgrade` refuses a seed made with a different LCA.

| Flag | Meaning |
|---|---|
| `--seed` | The seed image, `<vsno>:<version>`, or a vSNO (required) |
| `--seed-version` | The seed's OpenShift version, for an image outside the environment's registry |
| `--to prep\|upgrade` | Stop after this stage (default `upgrade`) |
| `--finalize`, `--rollback` | After the upgrade, finalize it or roll it back |
| `--lca-ref`, `--lca-repo`, `--lca-image` | Run LCA built from a ref, or a prebuilt image, instead of the catalog's (also on `prepare`) |

## vsno run

```bash
vsno run                                        # shows the plan, asks, then does it all
vsno run --yes --finalize                       # for CI: no question, finalize at the end
vsno run --target-version 4.20.45 --seed-version 4.20.46
vsno run --seed-image registry/seeds/a:4.21.42  # no seed vSNO; upgrade to an existing seed
vsno run -f vsno.yaml                           # the same settings from a file
```

`run` does an Image Based Upgrade with only a kubeconfig:
1. **setup**, as `vsno setup`
2. **vsnos**: a seed and a target vSNO, both started before waiting for either
3. **seed**: a seed image from the seed vSNO, which is stopped afterwards
4. **upgrade**: the target upgraded with it, as `vsno upgrade`

By default the seed is the newest stable release of the host's OpenShift minor and the target the
release before it. `--seed-version` and `--target-version` take anything `vsno create` accepts, and
the target must be older. The plan (versions, names, LCA source and what happens afterwards) is
shown and confirmed first, unless `--yes`.

Each stage reuses what's already there: a vSNO of that name at the right version, the seed image an
earlier run made, an upgrade part way. So when a run is interrupted, running it again carries on;
the plan says which stages an earlier run finished. Stages are recorded in the environment's state
(`vsno status`). Ctrl+C stops a run cleanly.

A vSNO the run has to create is **restored from its snapshot** when the environment keeps one at the
plan's version (see [Snapshots](#snapshots)): a minute instead of an install, and the plan says so
(`target   vSNO target at 4.22.21, restored from snapshot target`). A snapshot is only usable under
its own name and version, so a run naming other vSNOs, or asking for another version, installs as
before. `vsno run --delete` leaves the snapshots behind, so the next run starts from them again.

`--restart` runs every stage again instead of skipping the ones this plan already finished, which is
what repeating a test wants. With the seed image kept (`--seed-image`) and a rollback at the end, a
target can be upgraded over and over without installing a vSNO:

```bash
vsno run --yes --rollback --restart --seed-image registry.vsno.svc.cluster.local:5000/seeds/seed:4.22.22
```

`-f vsno.yaml` takes the same settings as the flags, by their JSON names, and flags override it:

```yaml
targetVersion: "4.21.1"
seedVersion: "4.21.42"
lca: {ref: my-fix, repo: https://github.com/me/lifecycle-agent}
finalize: true
```

| Flag | Meaning |
|---|---|
| `--target-version`, `--seed-version` | The versions (default: the host minor's last two stable releases) |
| `--target-name`, `--seed-name` | vSNO names (default `target`, `seed`); existing ones are reused |
| `--seed-image` | Upgrade to this seed image; no seed vSNO |
| `--seed-repo` | Push the seed to `<repo>:<version>` instead of the environment's registry |
| `--lca-ref`, `--lca-repo`, `--lca-image` | LCA from a ref or an image on both vSNOs (see above) |
| `--node`, `--cores`, `--memory` | Where and how big the vSNOs are |
| `--finalize`, `--rollback`, `--delete` | What to do after the upgrade |
| `--yes` | Run the plan without asking |
| `-f <file>` | Settings file |

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
measured on cnfdt16: a 10 GiB volume snapshotted in 9s and restored in 3s, verified byte for byte.
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
of each ISO rather than two, `create` ejects the ISO once the vSNO is ready, and `isos prune`
removes what a failed or deleted create left behind.

## Release cache

```bash
vsno cache add 4.20.46            # mirror a release now
vsno cache list                   # versions, size, last used, the vSNOs using each
vsno cache prune --dry-run        # unused releases
vsno cache prune --keep 1 --yes   # ...removing all but the newest unused one
vsno cache gc                     # free the space of images already deleted
```

The environment's registry holds mirrored OpenShift releases under `ocp/release/<digest>`. `create`
mirrors a version the first time it needs it unless `--no-cache`; `cache add` does it ahead of time.
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

## status

```bash
vsno status            # table
vsno status -o json
```

`status` only reads. It shows:
- the host components: both operators, HyperConverged, the LVMCluster and StorageClass
- each operator with an update available, as a warning that still exits 0
- each support service with its readiness and Service IP
- the registry endpoint, and what the release cache holds (versions, size, and what is free)
- the VMs in the namespace
- the last five stages recorded in `vsno-state`

It exits non-zero when the namespace is missing or a component is missing or failing.

## teardown

```bash
vsno teardown --dry-run         # list what would be deleted
vsno teardown                   # delete the vsno namespace
vsno teardown --host --dry-run  # also list the host components
```

By default `teardown` deletes the environment namespace: its VMs, support services, volumes and
generated credentials. With `--host` it also removes, in this order:
1. HyperConverged
2. the boot-source image namespace
3. the OpenShift Virtualization Subscription, CSV and namespace
4. the `LVMCluster`, which releases the disk
5. the LVM Storage Subscription, CSV and namespace

It waits for each deletion before the next one.

It refuses, unless you pass `--force`, when:
- the namespace wasn't created by vsno (no `app.kubernetes.io/managed-by=vsno` label and no `vsno-state`)
- with `--host`: VMs exist in other namespaces, PVCs in other namespaces use LVMS storage, or LVMS
  has an `LVMCluster` that vsno didn't create

With `--force` the problems are still listed, marked `(ignored: --force)`, and the JSON output
has `"forced": true`.

Its output mirrors `setup`: the same steps in reverse (host steps show `kept` without `--host`), and
the same objects in reverse, each marked `delete` or `absent`. It adds what setup's manifests
don't include but teardown also deletes, such as the operators' CSVs. If deleting the namespace would
also delete VMs or PVCs that setup didn't create, the plan warns about them.

It shows that plan and asks you to type the namespace (or `host` with `--host`), unless you
pass `--yes`.

| Flag | Meaning |
|---|---|
| `--host` | Also remove the host components |
| `--dry-run` | Show the plan and change nothing |
| `--yes` | Don't ask for confirmation |
| `--force` | Skip the safety checks above |
| `--timeout <duration>` | How long to wait for each deletion (default `10m`) |

## Background

The procedure this tool automates was verified by hand on 2026-11-18: a full IBU from 4.20.45 to
4.20.46 between two KubeVirt vSNOs on an existing OCP 4.22 cluster. Several things learned that
day will be built into the later commands:
- publish only `api`, `api-int` and `*.apps` in DNS
- escape the systemd unit name for the `/var/lib/containers` partition correctly
  ([lifecycle-agent#10670](https://github.com/openshift-kni/lifecycle-agent/pull/10670))
- give the guests their own pod and service ranges, separate from the host's
- use SeaweedFS for S3, since MinIO no longer publishes public images

## Development

```bash
make build   # ./vsno, versioned from git tags
make install # build, then copy to $GOPATH/bin (or ~/go/bin)
make test
make lint    # golangci-lint; CI also runs gosec, govulncheck and gitleaks
```

Planned work is tracked in the
[roadmap issues](https://github.com/sebrandon1/vsno/issues?q=label%3Aroadmap).

## License

[Apache-2.0](LICENSE)
