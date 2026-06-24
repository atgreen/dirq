# Production policy: AAP-only exec.
#
# Ad-hoc shell is rejected on production hosts. Only approved Ansible Automation
# Platform job templates may run, and each may issue only the exact command it
# is expected to. Break-glass shell is therefore impossible without a policy
# change managed alongside the rest of the agent configuration.

package dirq.agent

default allow := false
default reason := "prod exec requires an approved AAP job template"

# Map of approved job template -> the exact command it is permitted to run.
approved_templates := {
	"restart-nginx": "systemctl restart nginx",
	"reload-app": "systemctl reload myapp",
}

allow if {
	input.operation == "exec"
	want := approved_templates[input.aap_job_template]
	input.command == want
}

# Non-prod hosts are out of scope for this file; deny so a misapplied prod
# policy can never silently allow.
