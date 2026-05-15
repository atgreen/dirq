# SPDX-License-Identifier: MIT
Name:           dirq
Version:        %{_version}
Release:        1%{?dist}
Summary:        DirQ CLI — command-line tool for querying the fleet
License:        MIT
URL:            https://github.com/atgreen/dirq

%description
DirQ command-line tool. Query agents, manage hosts, tokens, tags,
and generate TLS certificates.

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/share/licenses/dirq
install -m 0755 %{_sourcedir}/dirq %{buildroot}/usr/bin/dirq
install -m 0644 %{_sourcedir}/LICENSE %{buildroot}/usr/share/licenses/dirq/LICENSE

%files
/usr/bin/dirq
%license /usr/share/licenses/dirq/LICENSE
