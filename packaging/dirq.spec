# SPDX-License-Identifier: MIT
Name:           dirq
Version:        %{_version}
Release:        1%{?dist}
Summary:        DirQ CLI — command-line tool for querying the fleet
License:        MIT
URL:            https://github.com/atgreen/dirq
Source0:        dirq-%{_version}.tar.gz

BuildRequires:  golang >= 1.22

%description
DirQ command-line tool. Query agents, manage hosts, tokens, tags,
run ad-hoc commands across the fleet, and generate TLS certificates.

%prep
%setup -q -n dirq-%{_version}

%build
CGO_ENABLED=0 go build -ldflags "-X main.version=%{_version}" -o dirq ./cmd/dirq

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/share/licenses/dirq
install -m 0755 dirq %{buildroot}/usr/bin/dirq
install -m 0644 LICENSE %{buildroot}/usr/share/licenses/dirq/LICENSE

%files
/usr/bin/dirq
%license /usr/share/licenses/dirq/LICENSE
