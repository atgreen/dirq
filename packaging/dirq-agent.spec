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
mkdir -p %{buildroot}/usr/local/bin
mkdir -p %{buildroot}/usr/lib/systemd/system
cp %{_sourcedir}/dirq-agent %{buildroot}/usr/local/bin/dirq-agent
cp %{_sourcedir}/dirq-agent.service %{buildroot}/usr/lib/systemd/system/

%files
/usr/local/bin/dirq-agent
/usr/lib/systemd/system/dirq-agent.service

%post
systemctl daemon-reload

%preun
systemctl stop dirq-agent 2>/dev/null || true
systemctl disable dirq-agent 2>/dev/null || true
