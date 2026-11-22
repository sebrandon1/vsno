# vsno: how to use it

A copy-and-paste guide to `vsno`, which runs virtual single-node OpenShift clusters (vSNOs) as
KVM VMs in an OpenShift cluster you already have, and drives lifecycle-agent Image Based Upgrades
(IBUs) between them.

Everything here uses two placeholders: `<name>` for a vSNO you name (lowercase letters, digits and
`-`, up to 30 characters, starting with a letter) and `<version>` for an OpenShift version such as
`4.22.22` (a minor, a z-stream, `<minor>-nightly`, or a release image pullspec — all things `vsno create
--version` accepts).

## 0. Before you start

You need:

- a kubeconfig for the host cluster, with cluster-admin, exported or passed per command:

  ```bash
  export KUBECONFIG=~/kubeconfigs/host-cluster
  ```

- one worker node with `/dev/kvm` and enough memory for the vSNOs (ideally bare metal), and a
  disk free for LVM Storage
- `oc` on your `PATH` (vsno shells it out for precheck)

All commands accept these:

| Flag | Meaning |
|---|---|
| `--kubeconfig <path>` | the host cluster (default: `$KUBECONFIG`, then `~/.kube/config`) |
| `--context <name>` | kubeconfig context |
| `-n, --namespace <ns>` | the vSNO environment's namespace (default `vsno`) |
| `-o table\|wide\|json` | output format |
| `--no-color` | plain output (also `NO_COLOR=1`) |

## 1. Check the cluster can host vSNOs

```bash
vsno precheck
```

It reports what it found and what to change, and exits non-zero when something is missing. When it
passes, save the suggested settings:

```bash
vsno precheck --write-config vsno.yaml
```

`vsno.yaml` holds the node, the LVMS disk and the sizes precheck suggests. `setup` reads it with
`--config`; `run` takes the same choices from its own flags (`--node`, `--disk`, `--cores`,
`--memory`).

## 2. Set up the environment

See what setup would do first:

```bash
vsno setup --dry-run
```

Then run it:

```bash
vsno setup --config vsno.yaml
```

Setup installs OpenShift Virtualization and LVM Storage if they are not already there (reusing them
when they are), puts an LVMCluster on the chosen worker's disk, and creates the environment
namespace with what the vSNOs need: dnsmasq for guest DNS, a builder pod, S3 for OADP and the
in-cluster registry that holds mirrored releases. It waits until all of it is ready.

Useful options:

```bash
vsno setup --node <worker>                  # pick the node explicitly
vsno setup --disk /dev/disk/by-id/<id>      # fresh disk for LVMS
vsno setup --registry-size 200Gi            # room for more mirrored releases
vsno setup --memory-overcommit 150          # fit more vSNOs (changes every VM on the host)
```

`setup` only takes over a namespace vsno did not create when you say `--force`, so it cannot
overwrite a hand-built environment by accident.

Check it any time:

```bash
vsno status
```

`status` changes nothing and exits non-zero when the environment is not ready.

## 3. Create your first vSNO

```bash
vsno create <name> --version 4.22 --watch
```

`--watch` follows the install with a progress bar until the vSNO's cluster is ready (about 20–45
minutes: the agent installer runs inside the VM). Without it, `create` returns once the VM exists
and you can follow along later:

```bash
vsno list --watch
```

`list` shows every vSNO with its state, version, size, age, API and a DETAIL column. The states:

| State | Meaning |
|---|---|
| `building-iso` | the agent ISO is being built in the builder pod |
| `importing-iso` | CDI is copying the ISO into the cluster |
| `starting` | the VM is scheduling or booting |
| `installing` | the agent installer runs (with its progress) |
| `finishing` | the guest API is up and the control plane is rolling out |
| `making-seed` | a seed image is being generated; the guest's services are stopped for it |
| `ready` | the vSNO's cluster is up |
| `stopped` | stopped on purpose (`vsno stop`) |
| `failed` | the install or the VM failed; DETAIL says why |
| `deleting` | being deleted |

Useful options:

```bash
vsno create <name> --version 4.22 --memory 32 --cores 16 --disk 200   # size it
vsno create <name> --version 4.22-nightly                            # the latest CI nightly of a minor
vsno create <name> --version quay.io/openshift-release-dev/ocp-release@sha256:...  # a pullspec
vsno create <name> --version 4.22 --no-cache                           # pull from the internet
```

The first vSNO of a version also mirrors that release's payload into the environment's registry
(15–20 GB, once); later vSNOs of it install from there. `vsno cache list` shows what is mirrored.

## 4. Work with a vSNO

The vSNO's API (`https://api.<name>.vsno.lab:6443`) resolves only inside the environment, so use
vsno's helpers rather than a saved kubeconfig:

```bash
vsno list                                     # state, version, API, node capacity
vsno oc <name> get nodes -o wide              # oc as kubeadmin, through the builder
vsno oc <name> -- get clusterversion
vsno ssh <name>                               # a shell as core
vsno ssh <name> sudo crictl ps              # ...or one command
vsno kubeconfig <name> > /tmp/<name>.kubeconfig   # the admin kubeconfig (use it in-cluster)
vsno screenshot <name> --file <name>.png      # the VGA console, when the guest won't answer
```

Stop and start it (stopping frees its memory and keeps the disk):

```bash
vsno stop <name> --wait
vsno start <name> --wait
```

Delete it (keeps any snapshot, see §6):

```bash
vsno delete <name> --dry-run     # what it would remove
vsno delete <name>               # asks for the name
vsno delete <name> --yes         # doesn't ask
```

## 5. Run an Image Based Upgrade

The short way — one command does setup, both vSNOs, the seed image and the upgrade:

```bash
vsno run
```

It shows a plan and asks before doing anything. Add `--yes` for CI, and `--finalize` to finalize
the upgrade at the end:

```bash
vsno run --yes --finalize
vsno run --target-version 4.22.21 --seed-version 4.22.22
vsno run --seed-image registry.vsno.svc.cluster.local:5000/seeds/<name>:4.22.22
```

By default the seed is the newest stable release of the host's OpenShift minor and the target the
release before it. Each stage reuses what is already there, so an interrupted run continues where it
stopped; `--restart` runs every stage again (a repeat test wants that), and stages are recorded in
`vsno status`.

The long way — each step on its own, which is what you want when testing something in one stage:

```bash
vsno prepare seed --role seed           # LCA and registry trust on the seed vSNO
vsno seed create seed --stop            # make the seed image, then stop the seed vSNO
vsno prepare target                     # LCA, registry trust and an OADP backup location
vsno upgrade target --seed seed         # IBU the target with the seed's newest image
```

`upgrade` stops after the upgrade stage; finish it, or undo it:

```bash
vsno upgrade target --seed seed --finalize
vsno upgrade target --rollback
vsno upgrade target --abort             # cancel a Prep, back to Idle
```

`vsno seed list` shows the seed images, and `vsno seed delete <name>:<version>` removes one.

## 6. Go faster

**Snapshots.** A vSNO's installed disk can be kept and restored in about a minute instead of a
20–45 minute install:

```bash
vsno snapshot create <name>            # stops the vSNO, snapshots its disk, starts it again
vsno snapshot list                     # what is kept, and whether its vSNO still exists
vsno delete <name> --yes               # the snapshot stays
vsno create <name> --from-snapshot --watch
vsno snapshot delete <name> --yes      # free the space
```

Two rules: a disk carries the cluster's identity, so `--from-snapshot` restores **the same name**
only; and the vSNO **stops** for the snapshot (a guest cannot freeze its filesystems, so the
snapshot is taken with it off). `vsno run` uses a snapshot automatically when one matches the vSNO
name and version it is about to create.

**The release cache.** Mirrored releases install in a fraction of the time. `run` mirrors the
releases it needs at the same time, so a cold cache costs one mirror, not two:

```bash
vsno cache list                        # what is mirrored, its size, and which vSNOs use it
vsno cache prune --dry-run             # what nothing uses any more
vsno cache prune --yes
```

**Sizes.** 16 GiB fits two or three vSNOs on a worker; 32 GiB is what seed generation and the IBU's
precache want on a busy node.

## 7. Test a lifecycle-agent change

Point both vSNOs at an LCA built from a branch, tag or SHA (vsno builds it in the cluster and
deploys it), or at a prebuilt image:

```bash
vsno run --lca-ref my-fix --lca-repo https://github.com/me/lifecycle-agent
vsno run --lca-image quay.io/me/lifecycle-agent:test
```

For the step-by-step flow the same flags exist on `prepare` and `upgrade`:

```bash
vsno prepare seed --role seed --lca-ref my-fix
vsno seed create seed --stop
vsno upgrade target --seed seed --lca-image quay.io/me/lifecycle-agent:test
```

## 8. Clean up

```bash
vsno iso list                    # ISOs, installers and DataVolumes, and what uses them
vsno iso prune --dry-run         # things nothing uses (a finished install's ISO, old installers)
vsno iso prune --yes
vsno teardown --dry-run          # the whole environment: VMs, services, volumes
vsno teardown                    # asks you to type the namespace
```

`teardown --host` also removes what setup installed on the host cluster (HyperConverged, the
LVMCluster and both operators). It asks first, and it is not a dry run — check its plan before
saying yes.

## 9. When something looks wrong

```bash
vsno status                      # components, support services, vSNOs, recent stages, cache
vsno list -o json                # every field, including the ClusterVersion's own message
vsno oc <name> -- get clusteroperators
vsno oc <name> -- get imagebasedupgrades
vsno oc <name> get pods -n openshift-lifecycle-agent
vsno screenshot <name> --file console.png
```

- `finishing` with "control plane rolling out" during a fresh install is normal; the CVO reports
  errors that clear themselves while it applies. After 20 minutes the DETAIL shows the CVO's
  message, which is also always in `-o json` (`cvMessage`) and in `vsno status`.
- `making-seed` means seed generation is running and the guest is deliberately silent.
- A vSNO that reads `failed`, or an install that never reaches `ready`, is in `vsno list`'s DETAIL.
  The agent installer's own log is on the guest:

  ```bash
  vsno ssh <name> sudo tail -100 /var/log/openshift-agent-install.log
  vsno screenshot <name> --file console.png     # when the guest won't answer
  ```
- An IBU refuses at Prep when the seed and the target do not agree on the cache (both installed
  from the registry mirror, or neither) or when the seed's version is not higher than the target's.
- If two environments exist, commands name the one they use and the others they found; `-n` picks.

## 10. Command overview

| Command | What it does |
|---|---|
| `precheck` | checks the host cluster can host vSNOs, and suggests settings |
| `setup` | installs OpenShift Virtualization, LVM Storage and the environment |
| `status` | reports the environment's state without changing it |
| `create` | creates a vSNO (`--version`, `--from-snapshot`, `--watch`) |
| `list` | lists vSNOs and the node's remaining capacity |
| `start`, `stop` | power a vSNO while keeping its disk |
| `delete` | removes vSNOs |
| `oc`, `ssh`, `kubeconfig`, `screenshot` | reach into a vSNO |
| `prepare` | installs LCA (and OADP, registry trust) for an IBU |
| `seed` | makes, lists and deletes seed images |
| `upgrade` | runs, finalizes, rolls back or aborts an IBU on a target |
| `run` | the whole IBU from a kubeconfig, resumable, with `--restart` |
| `snapshot` | keeps a vSNO's disk and restores a vSNO from it |
| `cache` | lists and prunes the mirrored releases |
| `iso` | lists, deletes and prunes what installs leave behind |
| `teardown` | removes the environment (and with `--host`, what setup installed) |
| `version` | prints vsno's version |

Every command takes `--help` for its own flags and examples.
