#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Runs a command with DIRQ_TEST_POSTGRES_URL pointing at a throwaway
# PostgreSQL container, then tears the container down. This is what
# makes `make test-postgres` reproduce the CI postgres job on a laptop:
# CI supplies the database as a service container and sets the same
# variable, so both paths run the identical `go test` command.
#
#   ./releng/with-postgres.sh go test ./...
#
# If DIRQ_TEST_POSTGRES_URL is already set, the command runs against
# that database and no container is started.

set -euo pipefail

# Pinned: an unpinned service image is a test fixture that changes
# under you, and it will change on a day you are debugging something
# else. Bump deliberately.
PG_IMAGE="${PG_IMAGE:-docker.io/library/postgres:17.11-alpine}"
READY_TIMEOUT="${READY_TIMEOUT:-60}"

if [ "$#" -eq 0 ]; then
	echo "usage: $0 <command> [args...]" >&2
	exit 64
fi

if [ -n "${DIRQ_TEST_POSTGRES_URL:-}" ]; then
	echo "using existing DIRQ_TEST_POSTGRES_URL" >&2
	exec "$@"
fi

RUNTIME="${CONTAINER_RUNTIME:-}"
if [ -z "$RUNTIME" ]; then
	for candidate in podman docker; do
		if command -v "$candidate" >/dev/null 2>&1; then
			RUNTIME="$candidate"
			break
		fi
	done
fi
if [ -z "$RUNTIME" ]; then
	echo "neither podman nor docker found; set DIRQ_TEST_POSTGRES_URL to use an existing database" >&2
	exit 69
fi

NAME="dirq-test-pg-$$"

cleanup() {
	"$RUNTIME" rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "starting $PG_IMAGE as $NAME (runtime: $RUNTIME)" >&2
# Port 0 on the host side lets the runtime pick a free port, so
# concurrent runs and a locally installed postgres don't collide.
"$RUNTIME" run -d --name "$NAME" \
	-e POSTGRES_PASSWORD=postgres \
	-e POSTGRES_USER=postgres \
	-e POSTGRES_DB=postgres \
	-p 127.0.0.1::5432 \
	"$PG_IMAGE" >/dev/null

# Resolve the host port the runtime assigned.
PORT="$("$RUNTIME" port "$NAME" 5432/tcp | head -1 | sed 's/.*://')"
if [ -z "$PORT" ]; then
	echo "could not determine mapped port for $NAME" >&2
	"$RUNTIME" logs "$NAME" >&2 || true
	exit 70
fi

# Poll for readiness against a deadline. Never sleep a fixed interval
# and hope — that fails on the day the runner is loaded, which is
# never the day you are looking at this job.
deadline=$((SECONDS + READY_TIMEOUT))
until "$RUNTIME" exec "$NAME" pg_isready -U postgres -d postgres >/dev/null 2>&1; do
	if [ "$SECONDS" -ge "$deadline" ]; then
		echo "postgres did not become ready within ${READY_TIMEOUT}s; container logs follow:" >&2
		"$RUNTIME" logs "$NAME" >&2 || true
		exit 75
	fi
	sleep 1
done

export DIRQ_TEST_POSTGRES_URL="postgres://postgres:postgres@127.0.0.1:${PORT}/postgres?sslmode=disable"
echo "postgres ready on 127.0.0.1:${PORT}" >&2

"$@"
