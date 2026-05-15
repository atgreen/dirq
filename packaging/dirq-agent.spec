# SPDX-License-Identifier: MIT
Name:           dirq-agent
Version:        %{_version}
Release:        1%{?dist}
Summary:        DirQ agent — endpoint agent for fleet management
License:        MIT
URL:            https://github.com/atgreen/dirq

%description
DirQ agent component. Lightweight agent that runs on managed Linux
servers, collects system data, relays queries through the P2P mesh,
and optionally executes commands.

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/lib/systemd/system
mkdir -p %{buildroot}/etc/dirq
mkdir -p %{buildroot}/usr/share/licenses/dirq-agent
install -m 0755 %{_sourcedir}/dirq-agent %{buildroot}/usr/bin/dirq-agent
install -m 0644 %{_sourcedir}/dirq-agent.service %{buildroot}/usr/lib/systemd/system/
install -m 0644 %{_sourcedir}/agent.conf %{buildroot}/etc/dirq/agent.conf
install -m 0644 %{_sourcedir}/LICENSE %{buildroot}/usr/share/licenses/dirq-agent/LICENSE

%files
/usr/bin/dirq-agent
%license /usr/share/licenses/dirq-agent/LICENSE
/usr/lib/systemd/system/dirq-agent.service
%config(noreplace) /etc/dirq/agent.conf

%post
systemctl daemon-reload

%preun
systemctl stop dirq-agent 2>/dev/null || true
systemctl disable dirq-agent 2>/dev/null || true
