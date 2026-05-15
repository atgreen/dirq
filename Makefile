# SPDX-License-Identifier: MIT

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X main.version=$(VERSION)
BINDIR    := bin
DESTDIR   ?=
PREFIX    ?= /usr/local

CMDS := dirq-server dirq-agent dirq

.PHONY: build test lint clean install proto collection cross demo demo-down demo-logs help

.DEFAULT_GOAL := help

build: $(addprefix $(BINDIR)/,$(CMDS))  ## Build all binaries to bin/

$(BINDIR)/%: cmd/%/*.go internal/**/*.go
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/$*

test:  ## Run all tests
	go test ./...

lint:  ## Run golangci-lint
	golangci-lint run ./...

clean:  ## Remove build artifacts
	rm -rf $(BINDIR) dist/
	rm -f collection/atgreen/dirq/atgreen-dirq-*.tar.gz

install: build  ## Install binaries to DESTDIR/PREFIX/bin
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BINDIR)/dirq-server $(DESTDIR)$(PREFIX)/bin/
	install -m 0755 $(BINDIR)/dirq-agent  $(DESTDIR)$(PREFIX)/bin/
	install -m 0755 $(BINDIR)/dirq        $(DESTDIR)$(PREFIX)/bin/

proto:  ## Regenerate protobuf Go code
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       proto/dirq/v1/dirq.proto

collection:  ## Build Ansible collection tarball
	cd collection/atgreen/dirq && ansible-galaxy collection build --force

cross:  ## Cross-compile for all release platforms
	@mkdir -p dist
	@for GOARCH in amd64 arm64; do \
		echo "Building linux/$$GOARCH..."; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$GOARCH go build -ldflags "$(LDFLAGS)" -o dist/dirq-server-linux-$$GOARCH ./cmd/dirq-server; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$GOARCH go build -ldflags "$(LDFLAGS)" -o dist/dirq-agent-linux-$$GOARCH  ./cmd/dirq-agent; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$GOARCH go build -ldflags "$(LDFLAGS)" -o dist/dirq-linux-$$GOARCH        ./cmd/dirq; \
	done
	@echo "Building windows/amd64..."
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/dirq-agent-windows-amd64.exe ./cmd/dirq-agent
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/dirq-windows-amd64.exe      ./cmd/dirq
	@echo "Building darwin/amd64 darwin/arm64..."
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/dirq-darwin-amd64 ./cmd/dirq
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/dirq-darwin-arm64 ./cmd/dirq

demo: demo-down build  ## Start local demo (server + 10 agents)
	podman build --target server -t localhost/dirq-server:dev .
	podman build --target agent  -t localhost/dirq-agent:dev .
	-podman network create dirq-demo 2>/dev/null || true
	podman kube play --network dirq-demo demo.yml
	@echo
	@echo "Waiting for fleet to register..."
	@sleep 6
	@echo
	@echo "Demo fleet running (10 agents):"
	@echo "  Server:  http://localhost:19080"
	@echo "  gRPC:    localhost:19051"
	@echo
	@echo "Try:"
	@echo "  export DIRQ_SERVER_URL=http://localhost:19080"
	@echo "  ./bin/dirq hosts list"
	@echo "  ./bin/dirq select hostname, os_info.os, cpu.logical_cores"
	@echo "  ./bin/dirq exec \"uptime\""
	@echo "  ./bin/dirq exec \"hostname\" WHERE tag.env = 'prod'"
	@echo "  ./bin/dirq select hostname WHERE tag.role = 'database'"
	@echo "  ./bin/dirq doctor"
	@echo
	@echo "Stop with: make demo-down"

demo-down:  ## Stop the demo fleet
	-podman kube down demo.yml 2>/dev/null || true
	-podman pod ls --format '{{.Name}}' | grep -E '^dirq-|^demo-' | xargs -r podman pod rm -f 2>/dev/null || true
	-podman network rm dirq-demo 2>/dev/null || true

demo-logs:  ## Tail demo server logs
	podman logs -f dirq-server-server

help:  ## Show this help
	@grep -E '^[a-z][-a-z]+:.*##' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'
