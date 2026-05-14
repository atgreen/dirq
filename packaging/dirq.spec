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
mkdir -p %{buildroot}/usr/local/bin
cp %{_sourcedir}/dirq %{buildroot}/usr/local/bin/dirq

%files
/usr/local/bin/dirq
