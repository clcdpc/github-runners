# CLC GitHub Actions runners

Disposable Ubuntu 24.04 runners managed by a Windows controller and Hyper-V.
Each VM runs one job, including Docker workloads, then is deleted.

Defaults: **3 concurrent runners, 2 vCPUs, 10 GiB RAM and 20 GiB logical disk per
VM**, with zero idle runners. Workflow label: `clc-linux`.

## Setup

Requires Windows x64 with Hyper-V, PowerShell 7.4+, internet access and
[sufficient capacity](docs/operations.md#capacity-and-storage). If Windows runs
inside a VM, its host must expose nested virtualization.

For a dedicated non-admin local account, an administrator can run
`.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners -CreateLocalUser`
from a fresh checkout, then sign in as that user and run activation below.
See [account preparation](docs/operations.md#prepare-a-dedicated-non-admin-account)
for existing accounts, storage selection and a dry run.

For setup under your current Windows account, run from this checkout.
First, in elevated PowerShell 7:

```powershell
.\runners.ps1 PrepareHost
```

Follow any restart or sign-out instructions, [create a GitHub PAT](#github-personal-access-token),
then use normal PowerShell 7 under the controller account:

```powershell
.\runners.ps1 Activate -StartupMode Password
```

Activation prompts for the [GitHub PAT](#github-personal-access-token)
and Windows account password when needed, builds the Ubuntu image, and starts the
controller. Allow up to 90 minutes for
the first image build; later activations reuse compatible images.

Password startup runs at boot without anyone signed in. See
[startup details](docs/operations.md#unattended-boot-startup) for account
requirements and password changes. Use `Activate -NoStartup` to defer startup
registration.

### GitHub personal access token

Use an **organization-scoped fine-grained personal access token (PAT)**, starting
with `github_pat_`. This application does not accept classic PATs (`ghp_`). The
GitHub account that creates the token must have permission to manage the
organization's runners; it is separate from the local Windows `github-runners`
account.

1. Open GitHub **Settings → Developer settings → Personal access tokens → Fine-grained
   tokens → Generate new token**, or use the [token creation page](https://github.com/settings/personal-access-tokens/new).
2. Give it a descriptive name and an expiration that follows your organization's
   policy. Set **Resource owner** to `clcdpc`, or the organization in
   `github.organizationUrl` if using another configuration.
3. Under **Repository access**, select the repositories in `github.repositories`
   and any private repositories whose workflow/job details you want `Jobs` to show.
   Grant these permissions:

   | Scope | Permission | Access |
   | --- | --- | --- |
   | Organization | Administration | Read-only |
   | Organization | Self-hosted runners | Read and write |
   | Repository | Actions | Read-only; optional for private-repository job details |

4. Generate the token and complete any required organization approval/SSO steps.
   A token awaiting approval cannot access private organization resources. See
   [GitHub's token creation guide](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token)
   and [runner authentication permissions](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api#authenticating-arc-with-a-fine-grained-personal-access-token).

While signed in as the Windows account that will run the controller, save the token
from normal PowerShell 7, then activate:

```powershell
.\runners.ps1 SetToken
.\runners.ps1 Activate -StartupMode Password
```

`SetToken` prompts with hidden input and encrypts the saved token using Windows
CurrentUser DPAPI. It must run as the controller account because another Windows
user cannot decrypt that credential. Activation also prompts for a missing PAT,
so the separate `SetToken` step is optional on first setup. To replace an expired
or revoked PAT, create a new one and rerun these commands. Supply the same
`-Config <path>` to both commands when using a custom configuration. For CLI import
and more details, see [credentials and GitHub identities](docs/operations.md#credentials-and-github-identities).

## Commands

Run these as `.\runners.ps1 <command>`:

| Command | Purpose |
| --- | --- |
| `Activate` | Install, update or recover |
| `Status` | Show controller health and capacity |
| `Jobs` / `Jobs -Json` | Show active jobs |
| `RestartWhenIdle` | Drain work and restart |
| `Stop` | Stop after active work finishes |
| `Start` | Start a prepared installation |
| `EnableStartup -StartupMode Password` | Enable unattended boot startup |
| `Cleanup` | Clean up owned resources while stopped |
| `Test` | Run unit tests, vet and PowerShell checks |
| `TestHyperV` | Run resource-intensive VM, Docker, SQL and browser checks |

## Configuration

Edit `local/controller.json` for GitHub access, concurrency, VM resources and
storage paths; see the [example](config/controller.example.json). Commands accept
`-Config <path>` for another configuration.

Set top-level `storageRoot` to choose the folder for VM data, images, logs and
credentials. It defaults to the configuration file's directory.

Keep runtime files in ignored `local/` storage or outside the checkout. Review
logs and diagnostic archives before sharing; see the [storage guidance](docs/operations.md#capacity-and-storage)
for custom paths and Git exclusions.

Runner VMs allow public IPv4 internet access and controller SSH. Private/local
networks and IPv6 are blocked; see the
[network policy](docs/operations.md#runner-network-restrictions).

- [Operations](docs/operations.md): setup, updates, image builds and recovery.
- [Design](docs/design.md): architecture and security.
- [Validation](docs/validation.md): checks, known blockers and acceptance evidence.
- [Runner diagnostics](docs/runner-diagnostics.md): retained evidence, privacy limits and caller attribution.
- [Disk benchmarks](benchmarks/hyperv-disk/README.md): harness and findings.
