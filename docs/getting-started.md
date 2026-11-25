# Getting started: install, precheck, setup

Install vsno, check that the host cluster can run vSNOs, and prepare it. Everything here is also in
the [How-To guide](howto.md) as a single walkthrough; this page is the detail behind it. Every flag
is listed in [Command reference](commands.md).

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

`vsno version` (or `vsno --version`) prints the build version. Global flags, accepted by every
command (`--kubeconfig`, `--context`, `-n`, `-o`, `-v/--verbose`, `--no-color`), are in the
[command reference](commands.md#global-flags). `-v` traces what vsno is doing underneath; it is not
the version, which is `--version`.

## precheck: can this cluster host vSNOs?

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
| Each worker | `/dev/kvm`, bare metal or nested virtualization, CPU threads, schedulable memory, and a disk it can prove blank (at least 600 GB) for LVMS |

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

The flags worth knowing: `--write-config vsno.yaml` saves the suggested settings, `-o json` gives a
machine-readable report, `--seed-repo <repo>` checks push credentials for an external seed registry,
`--node <name>` probes only that worker, and `--no-probe` skips the hardware probe. The rest
(`--vms`, `--vm-cores`, `--vm-memory`, `--min-disk`, `--probe-timeout`, `--probe-image`) change the
sizing requirements; all are in the [command reference](commands.md#vsno-precheck).

`precheck` exits non-zero if any check fails. When at least one worker is eligible, the
problems found on the other workers are shown as info rather than failures.

In `-o json`, each result has a `group` (`cluster`, `install` or `workers`), per-node results
have a `node`, and `nodes` lists each evaluated worker with `eligible`.

## setup: prepare the host cluster

```bash
vsno setup --dry-run                     # list what setup would do, change nothing
vsno setup                               # the node, disk and channels precheck suggests
```

The suggestion is usually right, so no settings file is needed. To review it first, or keep it for
a later run:

```bash
vsno precheck --write-config vsno.yaml   # the suggested node, disk and channels
vsno setup --config vsno.yaml
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
node are only warnings. Settings come from `--node`/`--device`, then `--config`, then the
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

`vsno doctor` picks up where `status` stops: it looks for the states that are not visible as a
failing component but make an environment look broken — a stuck IBU `Idle` condition, a seed
generation nothing is following, a vSNO left with `crio` and `kubelet` disabled, a `Released`
PersistentVolume, a registry under the seed floor — and prints the command that clears each.
`vsno report` writes the same picture as one JSON (or `--markdown`) document, with the credentials
named but never quoted.

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
