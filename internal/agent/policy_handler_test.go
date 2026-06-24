// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/atgreen/dirq/internal/agent/policy"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// captureStream is a fake upstream stream that records every AgentMessage the
// agent sends. Only Send/Recv are exercised by the handlers; the embedded
// grpc.ClientStream supplies the rest of the interface and panics if used.
type captureStream struct {
	grpc.ClientStream
	mu   sync.Mutex
	sent []*pb.AgentMessage
}

func (c *captureStream) Send(m *pb.AgentMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

func (c *captureStream) Recv() (*pb.ServerMessage, error) { return nil, io.EOF }

func (c *captureStream) messages() []*pb.AgentMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*pb.AgentMessage(nil), c.sent...)
}

// denyAllPolicy is a policy that denies every operation with a clear reason.
const denyAllPolicy = `package dirq.agent

default allow := false
reason := "blocked by test policy"
`

func newTestAgent(t *testing.T, policySrc string) (*Agent, *captureStream) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.rego")
	if err := os.WriteFile(path, []byte(policySrc), 0600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	eng, err := policy.New(context.Background(), policy.Config{File: path, FailClosed: true})
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}

	cs := &captureStream{}
	a := &Agent{
		cfg:            Config{ExecEnabled: true},
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		hostname:       "test-host",
		agentID:        "agent-test",
		policyEngine:   eng,
		upstreamStream: cs,
	}
	return a, cs
}

func TestExecPolicyDenied(t *testing.T) {
	a, cs := newTestAgent(t, denyAllPolicy)

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId: "execm-1",
		AgentId:   a.agentID,
		Command:   "id",
	})

	msgs := cs.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	resp := msgs[0].GetExecResponse()
	if resp == nil {
		t.Fatal("expected ExecResponse")
	}
	if resp.Success {
		t.Fatal("denied exec must report Success=false")
	}
	if resp.Rc != -1 {
		t.Fatalf("denied exec Rc = %d, want -1", resp.Rc)
	}
	if !strings.HasPrefix(resp.Error, "policy denied:") {
		t.Fatalf("error %q lacks 'policy denied:' prefix", resp.Error)
	}
}

func TestPutFilePolicyDeniedNoWrite(t *testing.T) {
	a, cs := newTestAgent(t, denyAllPolicy)
	dest := filepath.Join(t.TempDir(), "should-not-exist.conf")

	a.handlePutFile(context.Background(), &pb.PutFileRequest{
		RequestId: "put-1",
		AgentId:   a.agentID,
		DestPath:  dest,
		Content:   []byte("hello"),
	})

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("denied put_file must not create the file")
	}
	resp := cs.messages()[0].GetFileChunk()
	if resp == nil || resp.Success {
		t.Fatal("expected failed FileChunk")
	}
	if !strings.HasPrefix(resp.Error, "policy denied") {
		t.Fatalf("error %q lacks policy-denied prefix", resp.Error)
	}
}

func TestPutFilePolicyAllowedWrites(t *testing.T) {
	// Allow put_file only under the test's temp dir; deny everything else.
	dir := t.TempDir()
	allowed := filepath.Join(dir, "ok")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatal(err)
	}
	policySrc := "package dirq.agent\n\ndefault allow := false\n\n" +
		"allow if {\n\tinput.operation == \"put_file\"\n\t" +
		"startswith(input.dest_path, \"" + allowed + "/\")\n}\n"
	a, cs := newTestAgent(t, policySrc)

	// A matching path is written.
	good := filepath.Join(allowed, "app.conf")
	a.handlePutFile(context.Background(), &pb.PutFileRequest{
		RequestId: "put-ok",
		AgentId:   a.agentID,
		DestPath:  good,
		Content:   []byte("hello"),
	})
	if b, err := os.ReadFile(good); err != nil || string(b) != "hello" {
		t.Fatalf("allowed put_file should write file: content=%q err=%v", b, err)
	}
	if resp := cs.messages()[0].GetFileChunk(); resp == nil || !resp.Success {
		t.Fatal("expected successful FileChunk for allowed path")
	}

	// A non-matching path is denied and never created.
	bad := filepath.Join(dir, "denied.conf")
	a.handlePutFile(context.Background(), &pb.PutFileRequest{
		RequestId: "put-bad",
		AgentId:   a.agentID,
		DestPath:  bad,
		Content:   []byte("x"),
	})
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("put_file outside the allowed prefix must be denied")
	}
}

func TestFetchFilePolicyDenied(t *testing.T) {
	a, cs := newTestAgent(t, denyAllPolicy)

	// Point at a real, readable file to prove the denial happens before any read.
	real := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(real, []byte("top secret"), 0600); err != nil {
		t.Fatal(err)
	}

	a.handleFetchFile(context.Background(), &pb.FetchFileRequest{
		RequestId: "fetch-1",
		AgentId:   a.agentID,
		SrcPath:   real,
	})

	resp := cs.messages()[0].GetFetchResponse()
	if resp == nil || resp.Success {
		t.Fatal("expected failed FetchFileResponse")
	}
	if len(resp.Content) != 0 {
		t.Fatal("denied fetch_file must not return content")
	}
	if !strings.HasPrefix(resp.Error, "policy denied") {
		t.Fatalf("error %q lacks policy-denied prefix", resp.Error)
	}
}

func TestDeployPolicyDeniedNoWrite(t *testing.T) {
	a, cs := newTestAgent(t, denyAllPolicy)
	dest := filepath.Join(t.TempDir(), "pkg.rpm")

	a.handleDeploy(context.Background(), &pb.DeployRequest{
		RequestId:      "deploy-1",
		DestPath:       dest,
		Content:        []byte("PACKAGE"),
		InstallCommand: "true",
	})

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("denied deploy must not write the package")
	}
	resp := cs.messages()[0].GetDeployResponse()
	if resp == nil || resp.Success {
		t.Fatal("expected failed DeployResponse")
	}
	if resp.Phase != "policy" {
		t.Fatalf("denied deploy Phase = %q, want \"policy\"", resp.Phase)
	}
}

// TestNoPolicyAllows confirms the default (no engine configured) path is
// unchanged: a nil/no-op engine lets operations through to their handler.
func TestNoPolicyUnchanged(t *testing.T) {
	cs := &captureStream{}
	a := &Agent{
		cfg:            Config{ExecEnabled: true},
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		hostname:       "test-host",
		agentID:        "agent-test",
		policyEngine:   policy.Nop(),
		upstreamStream: cs,
	}

	dec := a.evalPolicy(context.Background(), policy.Input{Operation: "exec"})
	if !dec.Allow {
		t.Fatal("no-op engine must allow")
	}
}
