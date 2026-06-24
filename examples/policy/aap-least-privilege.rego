# Least-privilege AAP automation — segregation of duties.
#
# Each automation service account may run only the job templates assigned to
# it: the patching robot cannot deploy, the deploy robot cannot touch
# databases. Models the kind of role separation a bank's change-management
# process requires.
#
# Built for a change-managed environment, so every operation must carry AAP
# attribution. An operator who obtains a raw admin token and goes around the
# automation platform has no attribution and is denied — all change must flow
# through AAP.
#
# Enable with:
#   policy_file: /etc/dirq/policy.rego
#   policy_fail_closed: true

package dirq.agent

default allow := false
default reason := "no matching AAP authorization"

# ── Authorization matrix ──────────────────────────────────────────────
# Each automation account maps to the set of job templates it may run.
account_templates := {
	"svc-ansible-patching": {"patch-os", "restart-services"},
	"svc-ansible-deploy": {"deploy-app", "rollback-app"},
	"svc-ansible-dba": {"db-backup", "db-restart"},
}

# Templates permitted to escalate privilege (become=true). A template absent
# here may run, but only without become.
privileged_templates := {"patch-os", "deploy-app", "db-restart", "db-backup"}

# ── Gate 1: every operation must come through AAP ─────────────────────
has_attribution if {
	input.aap_user != ""
	input.aap_job_template != ""
	input.aap_job_id != ""
}

# ── Gate 2: the account must be authorized for this template ──────────
account_authorized if {
	templates := account_templates[input.aap_user]
	templates[input.aap_job_template]
}

# ── Gate 3: privilege escalation only for privileged templates ────────
become_ok if not input.become

become_ok if {
	input.become
	privileged_templates[input.aap_job_template]
}

# ── Decision ──────────────────────────────────────────────────────────
allow if {
	has_attribution
	account_authorized
	become_ok
}

# ── Reasons (mutually exclusive — no rule conflict) ───────────────────
reason := "operation must be initiated through AAP (missing attribution)" if {
	not has_attribution
}

reason := sprintf("account %q is not authorized for template %q", [input.aap_user, input.aap_job_template]) if {
	has_attribution
	not account_authorized
}

reason := sprintf("template %q is not permitted to escalate privilege (become)", [input.aap_job_template]) if {
	has_attribution
	account_authorized
	not become_ok
}
