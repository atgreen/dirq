# Install DirQ from packages

This guide takes you from nothing to a running DirQ control plane with a
registered agent, using the signed packages — no building from source. If you
just want to try DirQ on one machine, the [laptop quick start](../tutorials/quick-start.md)
is faster; come here when you're installing on real hosts.

## What you install where

DirQ ships three packages. You do **not** install all of them on every host.

| Package | Install on | Provides |
|---------|-----------|----------|
| `dirq-server` | one control host (or a few, for [HA](../explanation/high-availability.md)) | gRPC service for agents, REST API, query engine, Ansible inventory |
| `dirq` | your workstation / admin hosts | the `dirq` CLI |
| `dirq-agent` | every managed host in the fleet | the agent that joins the mesh and runs queries/exec |

The server uses an embedded **SQLite** database by default, so you can stand up a
working control plane with no external database. Point `db_url` at PostgreSQL when
you need multiple server pods or durability guarantees SQLite can't give you.

Managed hosts need **no inbound SSH or WinRM**. The fleet does need a couple of
ports open between hosts — see [Network and ports](#network-and-ports) below when
you set up firewalls.

## 1. Add the package repository

=== "Fedora / RHEL / AlmaLinux"

    ```bash
    sudo tee /etc/yum.repos.d/dirq.repo <<'EOF'
    [dirq]
    name=DirQ
    baseurl=https://atgreen.github.io/dirq/rpm-repo
    gpgcheck=1
    gpgkey=https://atgreen.github.io/dirq/rpm-repo/RPM-GPG-KEY-dirq
    enabled=1
    EOF
    ```

    Verify the signing key before trusting the repo — import it and check the
    fingerprint against the one published on the
    [releases page](https://github.com/atgreen/dirq/releases):

    ```bash
    sudo rpm --import https://atgreen.github.io/dirq/rpm-repo/RPM-GPG-KEY-dirq
    rpm -q gpg-pubkey --qf '%{SUMMARY}\n' | grep -i dirq
    ```

=== "Debian / Ubuntu"

    ```bash
    curl -fsSL https://atgreen.github.io/dirq/deb-repo/dirq-archive-keyring.gpg \
      | sudo gpg --dearmor -o /usr/share/keyrings/dirq-archive-keyring.gpg

    echo "deb [signed-by=/usr/share/keyrings/dirq-archive-keyring.gpg] \
      https://atgreen.github.io/dirq/deb-repo stable main" \
      | sudo tee /etc/apt/sources.list.d/dirq.list

    sudo apt update
    ```

    `apt` verifies every package against the keyring you installed above; a
    tampered or unsigned package fails the update.

## 2. Install and start the server

On the control host:

=== "Fedora / RHEL / AlmaLinux"

    ```bash
    sudo dnf install dirq-server dirq
    ```

=== "Debian / Ubuntu"

    ```bash
    sudo apt install dirq-server dirq
    ```

The `dirq-server` package installs the binary, a `dirq-server` systemd unit, and
a default config at `/etc/dirq/server.conf`. Edit that file before first start —
at minimum set a registration secret, and for anything beyond a trial, point at
PostgreSQL:

```
# /etc/dirq/server.conf
grpc_addr: :50051
http_addr: :8080
registration_secret: <a-strong-shared-secret>
# db_url: postgres://dirq:dirq@db.internal:5432/dirq?sslmode=require   # optional
```

The unit is **not** auto-enabled, so you choose when it starts:

```bash
sudo systemctl enable --now dirq-server
systemctl status dirq-server
```

!!! warning "Turn on TLS and auth before this server faces a real network"
    Both are on by default, but the quick start disables them. Do not copy the
    quick start's `DIRQ_TLS_DISABLED` / `DIRQ_AUTH_DISABLED` here. See
    [Enable TLS & authentication](tls-and-auth.md).

## 3. Get the bootstrap token and generated configs

On first start with no tokens, the server generates an admin **bootstrap token**
and writes ready-to-copy configs into `/var/lib/dirq`:

```bash
sudo cat /var/lib/dirq/bootstrap-token   # admin API token (also printed to the log)
ls /var/lib/dirq/
# agent.conf   client.conf   bootstrap-token   (plus the signing key, CA, and DB)
```

- **`/var/lib/dirq/client.conf`** — CLI config (server URL + bootstrap token).
- **`/var/lib/dirq/agent.conf`** — agent config with the server address,
  registration secret, and inline TLS certs. This is what every agent needs.

!!! danger "Persist `/var/lib/dirq`"
    It holds the server's Ed25519 signing key and CA. If it's ephemeral, a
    restart regenerates the key and **every registered agent rejects the server**
    until you re-distribute `agent.conf`. On a real deployment, put it on durable
    storage.

## 4. Configure the CLI

On your workstation, install the `dirq` package (step 2 if it isn't there), then
drop in the generated client config:

```bash
# copy from the server:
scp server:/var/lib/dirq/client.conf ~/.config/dirq/client.conf
dirq doctor
```

`dirq doctor` should report the server URL reachable and the API token valid.

## 5. Install agents

On every managed host, install the agent and drop in the server-generated config:

=== "Fedora / RHEL / AlmaLinux"

    ```bash
    sudo dnf install dirq-agent
    ```

=== "Debian / Ubuntu"

    ```bash
    sudo apt install dirq-agent
    ```

=== "Windows"

    Download and run the latest signed installer from the
    [releases page](https://github.com/atgreen/dirq/releases/latest):

    - **`dirq-agent-<version>.msi`** — for unattended / GPO deployment:

        ```powershell
        msiexec /i dirq-agent-<version>.msi /qn
        ```

    - **`dirq-agent-<version>-setup.exe`** — interactive installer.

    Prefer a standalone binary? Download `dirq-agent-windows-amd64.exe`
    (or `dirq-agent-windows-arm64.exe`) and register it as a Windows Service:

    ```powershell
    .\dirq-agent-windows-amd64.exe install
    ```

    Verify the download first with `Get-AuthenticodeSignature .\dirq-agent-<version>.msi`,
    or check it against `checksums.sha256` on the release.

Then distribute the config the server generated and start the service:

```bash
# copy /var/lib/dirq/agent.conf from the server to each host as:
#   Linux:   /etc/dirq/agent.conf
#   Windows: C:\ProgramData\dirq\agent.conf
sudo systemctl enable --now dirq-agent      # Linux
```

On Windows, restart the **dirq-agent** service after writing the config. Like the
server, the Linux agent unit is not auto-enabled — `enable --now` starts it and
sets it to start on boot.

## 6. Verify the fleet

Confirm each agent actually attached to the mesh, not just registered:

```bash
systemctl status dirq-agent                 # on the agent host: is it running?
sudo journalctl -u dirq-agent -n 50         # any registration / dial errors?

dirq doctor                                 # from your workstation
dirq hosts list                             # the new host should appear online
dirq debug ping <hostname>                  # proves a message reaches it end-to-end
dirq select hostname, cpu.logical_cores WHERE hostname = '<hostname>'
```

`dirq hosts list` showing a host "online" is necessary but not sufficient — a
*ghost-online* host registered but never attached to a relay parent. `dirq debug
ping` is the real proof. If it times out, see
[Diagnose the mesh](diagnostics.md).

## Network and ports

DirQ builds a relay tree, so any agent can be a *parent* for others and must
accept an inbound gRPC connection from its children. Open these between hosts:

| Connection | Port | Who initiates | Notes |
|------------|------|---------------|-------|
| Agent → Server | `50051/tcp` | agent | gRPC control channel (TLS) |
| Agent → Agent (relay) | `50052/tcp` | child agent | host-to-host within the fleet; a relay parent listens here |
| Admin/CLI → Server | `8080/tcp` | CLI | REST API (TLS) |
| Server → PostgreSQL | `5432/tcp` | server | only if you use external Postgres instead of SQLite |

Managed hosts still need **no** inbound SSH/WinRM. If an agent shows online but
`dirq debug ping` times out, `50052/tcp` host-to-host is usually the culprit —
see [Diagnose the mesh](diagnostics.md).

## Pin a version or upgrade

The commands above install the latest release. To install or hold a specific
version:

=== "Fedora / RHEL / AlmaLinux"

    ```bash
    sudo dnf install dirq-agent-0.25.0            # a specific version
    sudo dnf install python3-dnf-plugin-versionlock
    sudo dnf versionlock add dirq-agent           # freeze it
    ```

=== "Debian / Ubuntu"

    ```bash
    sudo apt install dirq-agent=0.25.0-1          # a specific version
    sudo apt-mark hold dirq-agent                 # freeze it
    ```

To upgrade, `apt-mark unhold` / `versionlock delete` then `dnf upgrade` /
`apt upgrade`. Keep the server at or ahead of the agents' version; `dirq doctor`
reports version skew.

## Uninstall

=== "Fedora / RHEL / AlmaLinux"

    ```bash
    sudo systemctl disable --now dirq-agent
    sudo dnf remove dirq-agent
    sudo rm -rf /etc/dirq /var/lib/dirq        # only if you want config + state gone
    ```

=== "Debian / Ubuntu"

    ```bash
    sudo systemctl disable --now dirq-agent
    sudo apt remove dirq-agent                 # or 'purge' to drop /etc/dirq too
    sudo rm -rf /var/lib/dirq
    ```

=== "Windows"

    Uninstall the MSI from **Settings → Apps**, or for the standalone binary:

    ```powershell
    .\dirq-agent-windows-amd64.exe uninstall
    ```

Removing the package stops the host reporting in, but the server still lists it
(offline). Remove it from the fleet with the admin API/CLI if you want it gone.
The same steps apply to `dirq-server` (substitute the name).

## Air-gapped / offline install

No internet on the target hosts? Mirror the repositories from a connected machine
and serve them internally:

- **RPM:** `reposync`/`dnf reposync` the `dirq` repo (or copy the RPMs from the
  [releases page](https://github.com/atgreen/dirq/releases) plus the GPG key),
  then `createrepo_c` and point an internal `baseurl` at it.
- **DEB:** mirror the `deb-repo` tree (or the `.deb` assets) behind an internal
  apt mirror with the keyring.
- **Windows:** copy the MSI to your software-distribution share.

Verify each artifact against `checksums.sha256` on the release before mirroring,
and distribute `agent.conf` over your existing secure channel.

## Next steps

- [Enable TLS & authentication](tls-and-auth.md) — required before production.
- [Deploy a production fleet](production-deployment.md) — the hardening checklist.
- [Configuration reference](../reference/configuration.md) — every key and env var.
