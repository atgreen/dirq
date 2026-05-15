# SPDX-License-Identifier: MIT
Name:           dirq-server
Version:        %{_version}
Release:        1%{?dist}
Summary:        DirQ server — Direct Query platform for fleet management
License:        MIT
URL:            https://github.com/atgreen/dirq
Source0:        dirq-%{_version}.tar.gz

BuildRequires:  golang >= 1.22, gcc

%description
DirQ server component. Provides gRPC service for agents, REST API for
admins, query engine, and Ansible inventory endpoint. Uses SQLite by
default (embedded); set DIRQ_DB_URL=postgres://... for PostgreSQL.

%prep
%setup -q -n dirq-%{_version}

%build
CGO_ENABLED=1 go build -ldflags "-X main.version=%{_version}" -o dirq-server ./cmd/dirq-server

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/lib/systemd/system
mkdir -p %{buildroot}/usr/share/licenses/dirq-server
install -m 0755 dirq-server %{buildroot}/usr/bin/dirq-server
install -m 0644 packaging/dirq-server.service %{buildroot}/usr/lib/systemd/system/
install -m 0644 LICENSE %{buildroot}/usr/share/licenses/dirq-server/LICENSE

%files
/usr/bin/dirq-server
/usr/lib/systemd/system/dirq-server.service
%license /usr/share/licenses/dirq-server/LICENSE

%post
systemctl daemon-reload

%preun
systemctl stop dirq-server 2>/dev/null || true
systemctl disable dirq-server 2>/dev/null || true
