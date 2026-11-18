# vsno

Run [lifecycle-agent](https://github.com/openshift-kni/lifecycle-agent) **Image Based
Upgrade (IBU)** tests on **virtual single-node OpenShift clusters (vSNOs)**. The vSNOs run as
OpenShift Virtualization VMs on a bare-metal worker of an OpenShift cluster you already have,
so you don't need a spare hypervisor or any reprovisioning.

> **Status:** milestone 1. `vsno precheck` works. `setup`, `teardown`, `status`, `create`,
> `prepare`, `seed generate`, `upgrade`, `run` and the guest helpers (`ssh`, `oc`,
> `kubeconfig`, `screenshot`) are planned. See the
> [roadmap issues](https://github.com/sebrandon1/vsno/issues?q=label%3Aroadmap).

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
| Existing state | OpenShift Virtualization or LVMS already installed, an existing `lvms-vg-vsno` StorageClass, an existing `vsno` namespace |
| Hub | Whether the cluster is managed by an ACM hub (hub policies may revert operators vsno installs) |
| Registry | The seed image goes to an in-cluster registry by default, so nothing is needed. Only if you pass the optional `--seed-repo` (e.g. `quay.io/<you>/ibu-seed`) does it check your local podman/docker auth files for push credentials. |
| Each worker | `/dev/kvm`, bare metal or nested virtualization, CPU threads, schedulable memory, and an empty disk of at least 600 GB for LVMS |

The per-worker hardware probe runs a short-lived privileged pod on each worker in a temporary
`vsno-precheck-*` namespace, the same way `oc debug node` does, and deletes it afterwards.
Nothing else is changed. Use `--no-probe` to skip the probe.

Example output (shortened) for a 3-master, 2-worker cluster where `worker-1` is a bare-metal
server and `worker-0` is a small VM:

```text
✓  Cluster access            cluster-admin
✓  OpenShift version         4.22.0
✓  Network overlap           host 10.128.0.0/14,172.30.0.0/16 vs guest 10.0.2.0/24,10.132.0.0/14,172.31.0.0/16
✓  Catalog: LVM Storage      lvms-operator channel stable-4.22
i  Seed registry             in-cluster registry (default); pass --seed-repo to push to an external registry instead
   ...
✓  worker-1: virtualization  bare metal (Example Server), /dev/kvm present
✓  worker-1: cpu             104 threads (need 32 for 2 VMs)
✓  worker-1: memory          80 GiB schedulable (need 66 GiB)
✓  worker-1: disk            sdb 1919GB empty: /dev/disk/by-id/wwn-0x5000000000000002
!  worker-0: virtualization  node is itself a VM (virt=kvm) with nested KVM
i  worker-0: cpu             not usable: 4 threads; one VM needs 16
✓  Eligible node             worker-1 (of 1 eligible)

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
make test
make lint    # golangci-lint; CI also runs gosec, govulncheck and gitleaks
```

Planned work is tracked in the
[roadmap issues](https://github.com/sebrandon1/vsno/issues?q=label%3Aroadmap).

## License

[Apache-2.0](LICENSE)
