# SPDX-License-Identifier: MIT
Name:           dirq-agent
Version:        %{_version}
Release:        1%{?dist}
Summary:        DirQ agent — endpoint agent for fleet management
License:        MIT
URL:            https://github.com/atgreen/dirq
Source0:        dirq-%{_version}.tar.gz

# Go is installed from upstream tarball in CI; no distro golang package needed.

%global debug_package %{nil}

%description
DirQ agent component. Lightweight agent that runs on managed Linux
servers, collects system data, relays queries through the P2P mesh,
and optionally executes commands.

%prep
%setup -q -n dirq-%{_version}

%build
CGO_ENABLED=0 go build -ldflags "-X main.version=%{_version}" -o dirq-agent ./cmd/dirq-agent

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/lib/systemd/system
mkdir -p %{buildroot}/etc/dirq
mkdir -p %{buildroot}/usr/share/licenses/dirq-agent
install -m 0755 dirq-agent %{buildroot}/usr/bin/dirq-agent
install -m 0644 packaging/dirq-agent.service %{buildroot}/usr/lib/systemd/system/
install -m 0644 packaging/agent.conf %{buildroot}/etc/dirq/agent.conf
install -m 0644 LICENSE %{buildroot}/usr/share/licenses/dirq-agent/LICENSE

%files
/usr/bin/dirq-agent
/usr/lib/systemd/system/dirq-agent.service
%config(noreplace) /etc/dirq/agent.conf
%license /usr/share/licenses/dirq-agent/LICENSE

%post
systemctl daemon-reload
# On upgrade ($1 == 2), restart if the service was already running.
# On fresh install ($1 == 1), don't start — agent.conf must be
# configured first.
if [ "$1" -ge 2 ]; then
    systemctl try-restart dirq-agent 2>/dev/null || true
fi

%preun
# On uninstall ($1 == 0), stop and disable.
# On upgrade ($1 == 1), don't stop — %post will restart.
if [ "$1" -eq 0 ]; then
    systemctl stop dirq-agent 2>/dev/null || true
    systemctl disable dirq-agent 2>/dev/null || true
fi
