# vsno

Create, list and delete **virtual single-node OpenShift clusters (vSNOs)** quickly, as many as
you need. The vSNOs run as OpenShift Virtualization VMs on a bare-metal worker of an OpenShift
cluster you already have, so you don't need a spare hypervisor or any reprovisioning.

One workflow built on top of that is testing
[lifecycle-agent](https://github.com/openshift-kni/lifecycle-agent) **Image Based Upgrades
(IBU)** between two vSNOs.

> **Status:** milestones 1–3 are done: `precheck`, `setup`, `status`, `teardown`, and the vSNO
> lifecycle (`create`, `list`, `delete`, `start`, `stop`, `oc`, `ssh`, `kubeconfig`, `screenshot`).
> Planned, in order:
> 1. fast creates from an in-cluster release cache
> 2. IBU (`prepare`, `seed generate`, `upgrade`) and testing lifecycle-agent builds
> 3. `vsno run`
>
> See the [roadmap issues](https://github.com/sebrandon1/vsno/issues?q=label%3Aroadmap).

## Install

Requires Go 1.27 or newer.

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
| Existing state | OpenShift Virtualization or LVMS already installed, an existing `lvms-vg-vsno` StorageClass, an existing environment namespace (`-n`, default `vsno`) |
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

Setup asks before taking over an existing namespace that vsno didn't create: one with neither the
`app.kubernetes.io/managed-by=vsno` label nor a `vsno-state` ConfigMap. It would overwrite objects
there, such as an environment built by hand, so it shows that warning and asks you to type the
namespace name to continue. Pass `--force` to take it over without asking, or use another
namespace with `-n`. If the namespace holds vSNOs, setup refuses without asking, since it would
fight the VMs already in it; only `--force` takes it over.

`--dry-run` lists each step (reuse what is installed, or apply) and each object, with what applying
it would do:
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
the environment `vsno setup` created, if there is exactly one; with several, pass `-n`.

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
| `--watch`, `-w` | Follow the install with progress until the vSNO is ready |
| `--timeout` | How long to wait for the install (default `90m`) |

`create` refuses a vSNO that doesn't fit next to what already runs on the node, and shows the free
memory. `vsno stop` frees a vSNO's memory and keeps its disk.

`list` shows every vSNO in every vsno environment, with a NAMESPACE column: the namespaces vsno
labeled, plus any that hold the builder pod (an environment assembled by hand predates the label).
`-n` narrows it to one. Each row has the vSNO's state, version, size, age and API, and the footer
says how many more default-size vSNOs fit on each node. The states are `importing-iso`, `starting`,
`installing` (with the installer's progress), `finishing`, `ready`, `stopped`, `failed` and
`deleting`.

Each vSNO's API is `https://api.<name>.vsno.lab:6443`. Only the environment resolves that name,
so `oc`, `ssh` and `kubeconfig` work through the builder pod. Everything after the vSNO's name goes
to `oc` or `ssh`, so put vsno's own flags, such as `-n`, before the name.

`delete` removes the VM, its disks, the ISO, its Service, its DNS entry and the builder's files for
it. It asks first unless you pass `--yes`, and `--dry-run` only lists what it would delete.

## Seed images

```bash
vsno seed create a                 # make seeds/a:4.20.46 from vSNO a
vsno seed create a --stop          # ...and stop a afterwards, to free its memory
vsno seed list                     # or ls, or -o json
vsno seed delete a:4.20.46 --dry-run
vsno seed delete a                 # all of a's seeds
```

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

`seed list` shows each seed image's reference, the vSNO it came from, its OpenShift version, size and
age. `seed delete` removes tags by full reference, `<vsno>:<version>` or `<vsno>`. It asks you to
type the arguments first unless `--yes`, and `--dry-run` only lists. The registry frees the disk
space only when it is garbage collected, so deletion does not shrink it immediately.

## status

```bash
vsno status            # table
vsno status -o json
```

`status` only reads. It shows:
- the host components: both operators, HyperConverged, the LVMCluster and StorageClass
- each support service with its readiness and Service IP
- the registry endpoint
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
