# Minimal allowlist policy for dirq-agent.
#
# Permits only unprivileged, non-production exec and log reads. Everything else
# is denied by default. A good starting point you can layer onto.
#
# Enable on an agent with:
#   policy_file: /etc/dirq/policy.rego
#   policy_fail_closed: true

package dirq.agent

default allow := false
default reason := "denied by default"

# Non-prod hosts may run unprivileged ad-hoc commands.
allow if {
	input.operation == "exec"
	input.tags.env != "prod"
	not input.become
}

# Diagnostic log reads, unprivileged only.
allow if {
	input.operation == "fetch_file"
	startswith(input.src_path, "/var/log/")
	not input.become
}

reason := "prod or privileged operations require an explicit allow rule" if {
	not allow
}
