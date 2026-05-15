# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

# ── Build stage ──────────────────────────────────────────
FROM docker.io/library/golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /dirq-server ./cmd/dirq-server
RUN CGO_ENABLED=0 go build -o /dirq-agent  ./cmd/dirq-agent
RUN CGO_ENABLED=0 go build -o /dirq        ./cmd/dirq

# ── Server image (minimal — no Python needed) ──────────
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest AS server

COPY --from=builder /dirq-server /usr/bin/dirq-server
COPY --from=builder /dirq        /usr/bin/dirq

EXPOSE 50051 8080

ENTRYPOINT ["dirq-server"]

# ── Agent image (full UBI — Python required for Ansible modules) ──
FROM registry.access.redhat.com/ubi9/ubi:latest AS agent

COPY --from=builder /dirq-agent /usr/bin/dirq-agent

EXPOSE 50052

ENTRYPOINT ["dirq-agent"]
