// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package policy

import (
	"context"
	"strings"
	"testing"
)

// loadExample builds an engine from a shipped example policy file.
func loadExample(t *testing.T, name string) Engine {
	t.Helper()
	e, err := New(context.Background(), Config{File: "../../../examples/policy/" + name})
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return e
}

func TestAAPLeastPrivilege(t *testing.T) {
	e := loadExample(t, "aap-least-privilege.rego")

	cases := []struct {
		name      string
		in        Input
		wantAllow bool
		reasonHas string // substring expected in reason when denied
	}{
		{
			name:      "patching account runs patch-os with become",
			in:        Input{Operation: "exec", AAPUser: "svc-ansible-patching", AAPJobTemplate: "patch-os", AAPJobID: "1", Become: true},
			wantAllow: true,
		},
		{
			name:      "patching account may not deploy",
			in:        Input{Operation: "exec", AAPUser: "svc-ansible-patching", AAPJobTemplate: "deploy-app", AAPJobID: "2"},
			wantAllow: false,
			reasonHas: "not authorized for template",
		},
		{
			name:      "deploy account deploys",
			in:        Input{Operation: "exec", AAPUser: "svc-ansible-deploy", AAPJobTemplate: "deploy-app", AAPJobID: "3"},
			wantAllow: true,
		},
		{
			name:      "no attribution is denied",
			in:        Input{Operation: "exec", Command: "id"},
			wantAllow: false,
			reasonHas: "must be initiated through AAP",
		},
		{
			name:      "unprivileged template may not become",
			in:        Input{Operation: "exec", AAPUser: "svc-ansible-patching", AAPJobTemplate: "restart-services", AAPJobID: "4", Become: true},
			wantAllow: false,
			reasonHas: "not permitted to escalate privilege",
		},
		{
			name:      "unknown account is denied",
			in:        Input{Operation: "exec", AAPUser: "mallory", AAPJobTemplate: "patch-os", AAPJobID: "5"},
			wantAllow: false,
			reasonHas: "not authorized",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := e.Eval(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("eval error (possible reason-rule conflict): %v", err)
			}
			if dec.Allow != tc.wantAllow {
				t.Fatalf("allow = %v, want %v (reason %q)", dec.Allow, tc.wantAllow, dec.Reason)
			}
			if !tc.wantAllow && tc.reasonHas != "" && !strings.Contains(dec.Reason, tc.reasonHas) {
				t.Fatalf("reason %q does not contain %q", dec.Reason, tc.reasonHas)
			}
		})
	}
}

func TestAAPBanking(t *testing.T) {
	e := loadExample(t, "aap-banking.rego")
	prodTags := map[string]string{"env": "prod"}
	pciTags := map[string]string{"scope": "pci"}

	cases := []struct {
		name      string
		in        Input
		wantAllow bool
		reasonHas string
	}{
		{
			name:      "prod account, approved template, prod host",
			in:        Input{Operation: "exec", Tags: prodTags, AAPUser: "svc-ansible-prod", AAPJobTemplate: "patch-os", AAPJobID: "1"},
			wantAllow: true,
		},
		{
			name:      "nonprod account rejected on prod host",
			in:        Input{Operation: "exec", Tags: prodTags, AAPUser: "svc-ansible-nonprod", AAPJobTemplate: "patch-os", AAPJobID: "2"},
			wantAllow: false,
			reasonHas: "high-assurance host",
		},
		{
			name:      "prod account, unapproved template, prod host",
			in:        Input{Operation: "exec", Tags: prodTags, AAPUser: "svc-ansible-prod", AAPJobTemplate: "rm-stuff", AAPJobID: "3"},
			wantAllow: false,
			reasonHas: "high-assurance host",
		},
		{
			name:      "PCI host is high-assurance too",
			in:        Input{Operation: "exec", Tags: pciTags, AAPUser: "svc-ansible-nonprod", AAPJobTemplate: "patch-os", AAPJobID: "4"},
			wantAllow: false,
			reasonHas: "high-assurance host",
		},
		{
			name:      "nonprod host allows any automation account",
			in:        Input{Operation: "exec", Tags: map[string]string{"env": "dev"}, AAPUser: "svc-ansible-nonprod", AAPJobTemplate: "whatever", AAPJobID: "5"},
			wantAllow: true,
		},
		{
			name:      "break-glass by approved responder is allowed",
			in:        Input{Operation: "exec", Tags: prodTags, AAPUser: "oncall-sre-lead", AAPJobTemplate: "break-glass-shell", AAPJobID: "6"},
			wantAllow: true,
		},
		{
			name:      "break-glass by wrong user denied and flagged",
			in:        Input{Operation: "exec", Tags: prodTags, AAPUser: "svc-ansible-nonprod", AAPJobTemplate: "break-glass-shell", AAPJobID: "7"},
			wantAllow: false,
			reasonHas: "BREAK-GLASS",
		},
		{
			name:      "fetch of credential store denied",
			in:        Input{Operation: "fetch_file", Tags: prodTags, AAPUser: "svc-ansible-prod", AAPJobTemplate: "collect-logs", AAPJobID: "8", SrcPath: "/etc/shadow"},
			wantAllow: false,
		},
		{
			name:      "fetch of /var/log allowed for automation account",
			in:        Input{Operation: "fetch_file", Tags: map[string]string{"env": "dev"}, AAPUser: "svc-ansible-nonprod", AAPJobTemplate: "collect-logs", AAPJobID: "9", SrcPath: "/var/log/app.log"},
			wantAllow: true,
		},
		{
			name:      "release deploy by prod account to staging dir",
			in:        Input{Operation: "deploy", Tags: prodTags, AAPUser: "svc-ansible-prod", AAPJobTemplate: "deploy-release", AAPJobID: "10", DestPath: "/var/tmp/dirq-deploy/app.rpm"},
			wantAllow: true,
		},
		{
			name:      "no attribution denied everywhere",
			in:        Input{Operation: "exec", Tags: prodTags, Command: "id"},
			wantAllow: false,
			reasonHas: "must be initiated through AAP",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := e.Eval(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("eval error (possible reason-rule conflict): %v", err)
			}
			if dec.Allow != tc.wantAllow {
				t.Fatalf("allow = %v, want %v (reason %q)", dec.Allow, tc.wantAllow, dec.Reason)
			}
			if !tc.wantAllow && tc.reasonHas != "" && !strings.Contains(dec.Reason, tc.reasonHas) {
				t.Fatalf("reason %q does not contain %q", dec.Reason, tc.reasonHas)
			}
		})
	}
}
