# vSNOs: create, use, delete

A vSNO is a single-node OpenShift cluster in a KubeVirt VM in the environment namespace. This page
covers its whole life; [Getting started](getting-started.md) sets the environment up and
[Upgrades](upgrades.md) uses two vSNOs for an Image Based Upgrade. Every flag is in the
[command reference](commands.md).

## The lifecycle

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

`create` refuses a vSNO that doesn't fit next to what already runs on the node, and shows the free
memory. `vsno stop` frees a vSNO's memory and keeps its disk.

The first `create` of a version also mirrors that release's payload into the environment's
registry (15–20 GB, once), so every later vSNO of it installs from the cluster: `--no-cache` skips
that. Either way the vSNO trusts the registry's CA, because LCA refuses a seed image whose trust
bundle differs from its target's, so a vSNO that skipped it could only make seeds for others like
it. A vSNO created with `--watch` records how long each phase took; `list -o wide` shows it.

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

## How it fits together

The procedure this tool automates was verified by hand on 2026-11-18: a full IBU from 4.20.45 to
4.20.46 between two KubeVirt vSNOs on an existing OCP 4.22 cluster. Several things learned that
day will be built into the later commands:
- publish only `api`, `api-int` and `*.apps` in DNS
- escape the systemd unit name for the `/var/lib/containers` partition correctly
  ([lifecycle-agent#10670](https://github.com/openshift-kni/lifecycle-agent/pull/10670))
- give the guests their own pod and service ranges, separate from the host's
- use SeaweedFS for S3, since MinIO no longer publishes public images
