# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

# ── Build stage ──────────────────────────────────────────
FROM docker.io/library/golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 go build -o /dirq-server ./cmd/dirq-server
RUN CGO_ENABLED=0 go build -o /dirq-agent  ./cmd/dirq-agent
RUN CGO_ENABLED=0 go build -o /dirq        ./cmd/dirq

# ── Server image (minimal — no Python needed) ──────────
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest AS server

COPY --from=builder /dirq-server /usr/bin/dirq-server
COPY --from=builder /dirq        /usr/bin/dirq

# The server needs no privileges: unprivileged ports, and nothing to write
# outside its state directory (dirq-632.10). Created here so the volume that
# backs it is owned by the same account the process runs as.
RUN mkdir -p /var/lib/dirq && chown 1001:0 /var/lib/dirq && chmod 0700 /var/lib/dirq
USER 1001

EXPOSE 50051 8080

ENTRYPOINT ["dirq-server"]

# ── Agent image (full UBI — Python required for Ansible modules) ──
# The agent image stays root: the agent runs operator-authorized commands on
# the host it manages and escalates with sudo for become. See the note in
# packaging/dirq-agent.service.
FROM registry.access.redhat.com/ubi9/ubi:latest AS agent

COPY --from=builder /dirq-agent /usr/bin/dirq-agent

EXPOSE 50052

ENTRYPOINT ["dirq-agent"]
