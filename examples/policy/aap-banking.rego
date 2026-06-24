# Regulated-bank agent policy, keyed on AAP attribution and host sensitivity.
#
# Control objectives:
#   * All change flows through AAP — operations without attribution are denied.
#   * Production and PCI-scoped hosts accept only change-approved templates,
#     run by the production automation account. Humans cannot ad-hoc on them.
#   * Privilege escalation (become) is restricted to named automation accounts.
#   * Secrets are never readable and writes are confined to managed config
#     directories, regardless of who asks.
#   * A single sanctioned break-glass template is permitted but flagged with a
#     distinct reason so the SIEM can alert and reviewers can find every use.
#
# Host sensitivity comes from agent tags (set in agent.conf): env=prod and
# scope=pci. Tag those hosts at provisioning time via your configuration
# management so the classification cannot be spoofed by the request.

package dirq.agent

default allow := false
default reason := "denied by default (no matching rule)"

# ── Roster ────────────────────────────────────────────────────────────
automation_accounts := {"svc-ansible-prod", "svc-ansible-nonprod"}
prod_account := "svc-ansible-prod"

# Change-approved templates permitted on high-assurance (prod/PCI) hosts.
prod_templates := {"patch-os", "restart-app", "rotate-certs", "deploy-release"}

# The one sanctioned emergency template, and who may invoke it.
break_glass_template := "break-glass-shell"
break_glass_users := {"oncall-sre-lead"}

# ── Host classification (from agent tags) ─────────────────────────────
high_assurance if input.tags.env == "prod"
high_assurance if input.tags.scope == "pci"

# ── Common gates ──────────────────────────────────────────────────────
has_attribution if {
	input.aap_user != ""
	input.aap_job_template != ""
	input.aap_job_id != ""
}

# Only automation accounts may escalate privilege.
become_ok if not input.become

become_ok if {
	input.become
	automation_accounts[input.aap_user]
}

# ── exec ──────────────────────────────────────────────────────────────
# High-assurance hosts: the prod automation account running an approved
# template, nothing else.
allow if {
	input.operation == "exec"
	has_attribution
	high_assurance
	input.aap_user == prod_account
	prod_templates[input.aap_job_template]
	become_ok
}

# Non-prod hosts: any automation account with attribution.
allow if {
	input.operation == "exec"
	has_attribution
	not high_assurance
	automation_accounts[input.aap_user]
	become_ok
}

# Break-glass: the emergency template by an approved responder, anywhere.
allow if {
	input.operation == "exec"
	has_attribution
	input.aap_job_template == break_glass_template
	break_glass_users[input.aap_user]
}

# ── put_file ──────────────────────────────────────────────────────────
# Writes confined to managed config directories, automation accounts only.
allow if {
	input.operation == "put_file"
	has_attribution
	automation_accounts[input.aap_user]
	managed_dest
	become_ok
}

managed_dest if startswith(input.dest_path, "/etc/myapp/")

managed_dest if startswith(input.dest_path, "/opt/app/conf/")

# ── fetch_file ────────────────────────────────────────────────────────
# Diagnostics under /var/log are readable; credential stores never are,
# regardless of account.
allow if {
	input.operation == "fetch_file"
	has_attribution
	automation_accounts[input.aap_user]
	startswith(input.src_path, "/var/log/")
	not sensitive_path
}

sensitive_path if {
	some prefix in ["/etc/shadow", "/etc/ssh/", "/root/.ssh/", "/etc/dirq/"]
	startswith(input.src_path, prefix)
}

# ── deploy ────────────────────────────────────────────────────────────
# NOTE: the DeployRequest proto does not carry aap_user to the agent, so a
# deploy policy here cannot see attribution. The server-side aap_user binding
# (require_aap_binding=true) is the authoritative attribution check for deploy.
# The agent policy adds a path constraint: deploys may only land in the managed
# staging directory.
allow if {
	input.operation == "deploy"
	startswith(input.dest_path, "/var/tmp/dirq-deploy/")
}

# ── Reasons (else-chain keeps them mutually exclusive — no conflict) ──
reason := "operation must be initiated through AAP (missing attribution)" if {
	not has_attribution
} else := "BREAK-GLASS: emergency template used — review required" if {
	input.aap_job_template == break_glass_template
} else := sprintf("%q on a high-assurance host requires the prod automation account and a change-approved template", [input.operation]) if {
	high_assurance
}
