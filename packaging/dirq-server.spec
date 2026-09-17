# SPDX-License-Identifier: MIT
Name:           dirq-server
Version:        %{_version}
Release:        1%{?dist}
Summary:        DirQ server — Direct Query platform for fleet management
License:        MIT
URL:            https://github.com/atgreen/dirq
Source0:        dirq-%{_version}.tar.gz

BuildRequires:  gcc

%global debug_package %{nil}

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
mkdir -p %{buildroot}/etc/dirq
mkdir -p %{buildroot}/usr/share/licenses/dirq-server
install -m 0755 dirq-server %{buildroot}/usr/bin/dirq-server
install -m 0644 packaging/dirq-server.service %{buildroot}/usr/lib/systemd/system/
install -m 0640 packaging/server.conf %{buildroot}/etc/dirq/server.conf
install -m 0644 LICENSE %{buildroot}/usr/share/licenses/dirq-server/LICENSE

%files
/usr/bin/dirq-server
/usr/lib/systemd/system/dirq-server.service
%config(noreplace) /etc/dirq/server.conf
%license /usr/share/licenses/dirq-server/LICENSE

%pre
# System account for the service. Created before %%files so the packaged
# paths can be owned correctly, and idempotent so upgrades are no-ops.
getent group dirq >/dev/null || groupadd -r dirq
getent passwd dirq >/dev/null || \
    useradd -r -g dirq -d /var/lib/dirq -s /sbin/nologin \
            -c "DirQ server" dirq
exit 0

%post
systemctl daemon-reload
# An install that predates the dirq user left /var/lib/dirq owned by root,
# and the service can no longer read its own signing key and TLS material.
# Hand the directory over rather than letting the upgrade fail to start.
if [ -d /var/lib/dirq ]; then
    chown -R dirq:dirq /var/lib/dirq || :
    chmod 0700 /var/lib/dirq || :
fi
if [ "$1" -ge 2 ]; then
    systemctl try-restart dirq-server 2>/dev/null || true
fi

%preun
if [ "$1" -eq 0 ]; then
    systemctl stop dirq-server 2>/dev/null || true
    systemctl disable dirq-server 2>/dev/null || true
fi
