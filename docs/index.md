# DirQ

**Direct Query Platform for Fleet Management & Ansible Execution.**

DirQ is an agent-based platform for querying and managing large Windows/Linux
fleets. Agents form a peer-to-peer relay mesh and report back to a central
server. The server acts as an Ansible Automation Platform (AAP) inventory
source, exposes collected data as structured facts, and can route Ansible
execution through the mesh as an alternative to SSH/WinRM.

- **Query the fleet like a dataset** instead of logging into hosts one by one.
- **Keep managed hosts outbound-only** instead of opening SSH/WinRM inbound.
- **Reuse Ansible** while replacing the transport underneath.
- **Build inventories from live query results** instead of static host lists.
- **Scale with a relay tree** so the server never needs a session to every node.
- **Scan for CVEs in real time** and run ad-hoc commands across the fleet.

## Start here

<div class="grid cards" markdown>

- :material-rocket-launch: **[Tutorial](tutorials/quick-start.md)** — get a mesh
  running on your laptop in five minutes.
- :material-wrench: **[How-to guides](how-to/production-deployment.md)** —
  task-focused recipes: deploy a fleet, build inventories, run playbooks, scan
  for CVEs, and more.
- :material-book-open-variant: **[Reference](reference/query-dsl.md)** — the
  Query DSL, configuration keys, REST API, CLI, and metrics.
- :material-lightbulb: **[Explanation](explanation/why-dirq.md)** — why DirQ
  exists and how the mesh, placement, and recovery work.

</div>

## Install the agent

DirQ publishes signed RPM and DEB packages for Linux, and signed installers for
Windows.

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

    sudo dnf install dirq-agent
    ```

=== "Debian / Ubuntu"

    ```bash
    curl -fsSL https://atgreen.github.io/dirq/deb-repo/dirq-archive-keyring.gpg \
      | sudo gpg --dearmor -o /usr/share/keyrings/dirq-archive-keyring.gpg

    echo "deb [signed-by=/usr/share/keyrings/dirq-archive-keyring.gpg] \
      https://atgreen.github.io/dirq/deb-repo stable main" \
      | sudo tee /etc/apt/sources.list.d/dirq.list

    sudo apt update
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

    The agent runs as a Windows Service (SYSTEM). After installing, edit
    `C:\ProgramData\dirq\agent.conf` and restart the **dirq-agent** service.

After installing on Linux, edit `/etc/dirq/agent.conf` and restart with
`sudo systemctl restart dirq-agent`. See
[Deploy a production fleet](how-to/production-deployment.md) for the full setup.
