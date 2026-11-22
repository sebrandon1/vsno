# vsno

Create, list and delete **virtual single-node OpenShift clusters (vSNOs)** quickly, as many as you
need. The vSNOs run as OpenShift Virtualization VMs on a bare-metal worker of an OpenShift cluster
you already have, so you don't need a spare hypervisor or any reprovisioning.

One workflow built on top of that is testing
[lifecycle-agent](https://github.com/openshift-kni/lifecycle-agent) **Image Based Upgrades (IBU)**
between two vSNOs.

> **Status:** all milestones are done: `precheck`, `setup`, `status`, `teardown`, the vSNO lifecycle
> (`create`, `list`, `delete`, `start`, `stop`, `oc`, `ssh`, `kubeconfig`, `screenshot`), seed
> images, fast creates from an in-cluster release cache, Image Based Upgrades (`prepare`, `upgrade`),
> testing lifecycle-agent branches in them, `vsno run` for the whole flow, and `vsno snapshot` to
> bring an installed vSNO back in about a minute instead of installing it.
>
> New here? Read the **[How-To guide](docs/howto.md)**: a copy-and-paste walkthrough from precheck
> to teardown. Planned work is tracked in the
> [roadmap issues](https://github.com/sebrandon1/vsno/issues?q=label%3Aroadmap).

## Quick start

```bash
go install github.com/sebrandon1/vsno@latest   # or download a release binary
vsno precheck                                   # can this cluster host vSNOs?
vsno precheck --write-config vsno.yaml          # ...and save the suggested settings
vsno setup --config vsno.yaml                   # install what vSNOs need
vsno create seed --version 4.22 --watch         # one vSNO; ~45 minutes
vsno list                                       # state, version, size, API
vsno run                                        # ...or the whole IBU from a kubeconfig
```

`vsno version` (or `vsno --version`) prints the build version.

## Commands

| Command | What it does |
|---|---|
| `vsno precheck` | Check whether this cluster can host vSNOs, and suggest settings |
| `vsno setup` | Install what vSNOs need on the host cluster |
| `vsno status` | Show the state of a vSNO environment |
| `vsno create [name] --version <v>` | Create a vSNO (`--from-snapshot` to restore one) |
| `vsno list` | List the vSNOs and what's left on their node |
| `vsno start` / `vsno stop` | Start or stop a vSNO, keeping its disk |
| `vsno delete` | Delete vSNOs |
| `vsno oc` / `vsno ssh` | Run `oc` against a vSNO, or open a shell on it |
| `vsno kubeconfig` / `vsno screenshot` | Print a vSNO's kubeconfig, or save its console |
| `vsno prepare` | Get vSNOs ready for an Image Based Upgrade |
| `vsno seed create` / `list` / `delete` / `precheck` | Make and manage seed images |
| `vsno upgrade` | Upgrade a vSNO with an Image Based Upgrade |
| `vsno run` | Do a whole Image Based Upgrade from only a kubeconfig |
| `vsno snapshot create` / `list` / `delete` | Keep a vSNO's disk, and create from it |
| `vsno cache add` / `list` / `prune` / `gc` | Manage the mirrored OpenShift releases |
| `vsno iso list` / `delete` / `eject` / `prune` | Clean up what creates leave behind |
| `vsno teardown` | Delete a vSNO environment |
| `vsno version` | Print the vsno version |

Every command's flags are in **[docs/commands.md](docs/commands.md)**.

## Guides

| Guide | Read it for |
|---|---|
| **[How-To](docs/howto.md)** | A copy-and-paste walkthrough from an empty kubeconfig to teardown |
| [Getting started](docs/getting-started.md) | Install, `precheck`, `setup`, `status` and `teardown` in depth |
| [vSNOs](docs/vsnos.md) | The vSNO lifecycle, the states, and reaching a guest |
| [Upgrades](docs/upgrades.md) | Seed images, `prepare`, `upgrade`, `vsno run`, and testing an LCA branch |
| [Performance](docs/performance.md) | Snapshots, the release cache, and ISOs |
| [Commands](docs/commands.md) | Every command and flag, as `--help` prints them |

## Requirements

- Go 1.27 or newer to build from source. Prebuilt binaries for Linux and macOS (amd64 or arm64)
  are on the [releases](https://github.com/sebrandon1/vsno/releases), with `checksums.sha256`.
- A kubeconfig for an OpenShift cluster with a bare-metal worker with `/dev/kvm`, enough CPU and
  memory for the vSNOs, and an empty disk of at least 600 GB for LVM Storage. `vsno precheck`
  checks all of it.

## Development

```bash
make build   # ./vsno, versioned from git tags
make install # build, then copy to $GOPATH/bin (or ~/go/bin)
make test
make lint    # golangci-lint; CI also runs gosec, govulncheck and gitleaks
```

## License

[Apache-2.0](LICENSE)
