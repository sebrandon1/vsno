# vsno

Create, list and delete **virtual single-node OpenShift clusters (vSNOs)** quickly, as many as you
need. The vSNOs run as OpenShift Virtualization VMs on a bare-metal worker of an OpenShift cluster
you already have, so you don't need a spare hypervisor or any reprovisioning.

One workflow built on top of that is testing
[lifecycle-agent](https://github.com/openshift-kni/lifecycle-agent) **Image Based Upgrades (IBU)**
between two vSNOs.

New here? The **[How-To guide](docs/howto.md)** is a copy-and-paste walkthrough. Bugs and ideas are
in the [issue tracker](https://github.com/sebrandon1/vsno/issues).

## Quick start

Nothing to configure: `setup` uses the node, disk and channels `precheck` suggests.

```bash
go install github.com/sebrandon1/vsno@latest   # or download a release binary

export KUBECONFIG=~/kubeconfigs/host-cluster   # the cluster that will host the vSNOs
vsno precheck                                  # can it host vSNOs?  ~1 minute
vsno setup                                     # install what they need; answer its prompts
vsno create seed --version 4.22 --watch        # first vSNO;  ~45 minutes
vsno list                                      # state, version, size, API, free capacity
```

Work on that vSNO with `vsno oc seed`, `vsno ssh seed` or `vsno kubeconfig seed`; or run a whole
IBU:

```bash
vsno run                    # a seed and a target vSNO, a seed image, the upgrade; shows the plan first
vsno run --yes --finalize   # for CI: no questions, and finalize at the end
```

To pin the choices instead of taking precheck's suggestion, pass them to `setup` (`--node worker-1
--device /dev/disk/by-id/<id>`), or save them once with `vsno precheck --write-config vsno.yaml` and
pass `--config vsno.yaml`.

`vsno version` (or `vsno --version`) prints the build version.

## Commands

| Command | What it does |
|---|---|
| `vsno precheck` | Check whether this cluster can host vSNOs, and suggest settings |
| `vsno setup` | Install what vSNOs need on the host cluster |
| `vsno status` | Show the state of a vSNO environment (`--watch` to refresh) |
| `vsno config` | Remember settings between runs: `namespace`, `output`, seed sync |
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
| `vsno doctor` | Find the states that make an environment look broken (`--fix` for the two safe ones) |
| `vsno report` | Write everything about the environment as one JSON or Markdown document |
| `vsno snapshot create` / `list` / `delete` | Keep a vSNO's disk, and create from it |
| `vsno cache add` / `list` / `prune` / `gc` | Manage the mirrored OpenShift releases |
| `vsno iso list` / `info` / `delete` / `eject` / `prune` | Clean up what creates leave behind |
| `vsno teardown` | Delete a vSNO environment |
| `vsno version` | Print the vsno version |

Every command's flags are in **[docs/commands.md](docs/commands.md)**.

## When something looks wrong

```bash
vsno status                 # components, support services, vSNOs, recent stages
vsno doctor                 # the known broken states, and the way out of each
vsno doctor --fix           # the two reversible fixes only; it never deletes
vsno report --markdown      # the whole environment in one document, for a bug report or a demo
```

`vsno doctor` names each state it knows — a stuck IBU `Idle` condition, a seed generation nothing is
following, a vSNO a stopped generation left with `crio` and `kubelet` disabled, a `Released`
PersistentVolume, a registry under the seed floor — and says how to clear it. The
[How-To guide](docs/howto.md#9-when-something-looks-wrong) has the rest.

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
make integration-test # runs the built binary and asserts on stdout, stderr and exit codes
make lint    # golangci-lint; CI also runs gosec, govulncheck and gitleaks
```

## License

[Apache-2.0](LICENSE)
