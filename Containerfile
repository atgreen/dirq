# ── Build stage ──────────────────────────────────────────
FROM docker.io/library/golang:1.24 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /dirq-server ./cmd/dirq-server
RUN CGO_ENABLED=0 go build -o /dirq-agent  ./cmd/dirq-agent
RUN CGO_ENABLED=0 go build -o /dirq        ./cmd/dirq

# ── Server image ────────────────────────────────────────
FROM docker.io/library/alpine:3.21 AS server

RUN apk add --no-cache ca-certificates
COPY --from=builder /dirq-server /usr/local/bin/dirq-server
COPY --from=builder /dirq        /usr/local/bin/dirq

EXPOSE 50051 8080

ENTRYPOINT ["dirq-server"]

# ── Agent image ─────────────────────────────────────────
FROM docker.io/library/alpine:3.21 AS agent

RUN apk add --no-cache ca-certificates
COPY --from=builder /dirq-agent /usr/local/bin/dirq-agent

EXPOSE 50052

ENTRYPOINT ["dirq-agent"]
