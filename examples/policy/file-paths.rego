# File transfer restriction policy.
#
# Writes are confined to an application's config directory; reads allow
# diagnostics but never credential stores. Path checks run on the agent's
# cleaned absolute path, so "/etc/myapp/../shadow" cannot slip through.

package dirq.agent

default allow := false
default reason := "file path not permitted by policy"

# Writes: only this app's config directory, .conf files, bounded size.
allow if {
	input.operation == "put_file"
	startswith(input.dest_path, "/etc/myapp/")
	endswith(input.dest_path, ".conf")
	input.content_size <= 1048576
}

# Reads: diagnostics under /var/log, but never sensitive credential locations.
allow if {
	input.operation == "fetch_file"
	startswith(input.src_path, "/var/log/")
	not sensitive_read
}

sensitive_prefixes := [
	"/etc/shadow",
	"/etc/ssh/",
	"/root/.ssh/",
]

sensitive_read if {
	some prefix in sensitive_prefixes
	startswith(input.src_path, prefix)
}
