# SPDX-License-Identifier: MIT
Name:           dirq-server
Version:        %{_version}
Release:        1%{?dist}
Summary:        DirQ server — Direct Query platform for fleet management
License:        MIT
URL:            https://github.com/atgreen/dirq

%description
DirQ server component. Provides gRPC service for agents, REST API for
admins, query engine, and Ansible inventory endpoint.

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/lib/systemd/system
mkdir -p %{buildroot}/usr/share/licenses/dirq-server
install -m 0755 %{_sourcedir}/dirq-server %{buildroot}/usr/bin/dirq-server
install -m 0644 %{_sourcedir}/dirq-server.service %{buildroot}/usr/lib/systemd/system/
install -m 0644 %{_sourcedir}/LICENSE %{buildroot}/usr/share/licenses/dirq-server/LICENSE

%files
/usr/bin/dirq-server
%license /usr/share/licenses/dirq-server/LICENSE
/usr/lib/systemd/system/dirq-server.service

%post
systemctl daemon-reload

%preun
systemctl stop dirq-server 2>/dev/null || true
systemctl disable dirq-server 2>/dev/null || true
