// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/atgreen/dirq/internal/agent/policy"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// newExecAgent returns an Agent with exec enabled and a no-op policy engine,
// wired to a captureStream (defined in policy_handler_test.go) so tests can
// inspect every response the handlers send upstream.
func newExecAgent(t *testing.T) (*Agent, *captureStream) {
	t.Helper()
	cs := &captureStream{}
	a := &Agent{
		cfg:            Config{ExecEnabled: true},
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		hostname:       "test-host",
		agentID:        "agent-test",
		policyEngine:   policy.Nop(),
		upstreamStream: cs,
	}
	return a, cs
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test exercises the Unix exec path")
	}
}

// ─────────────────────────────────────────────────────────
// Shell helpers
// ─────────────────────────────────────────────────────────

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "''"},
		{"abc", "'abc'"},
		{"a b", "'a b'"},
		{"a'b", `'a'\''b'`},
		{"''", `''\'''\'''`},
		{"$HOME; rm -rf /", "'$HOME; rm -rf /'"},
	}
	for _, tc := range tests {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapePowerShellArg(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"it's", "it''s"},
		{`say "hi"`, "say `\"hi`\""},
		{"a'b'c", "a''b''c"},
	}
	for _, tc := range tests {
		if got := escapePowerShellArg(tc.in); got != tc.want {
			t.Errorf("escapePowerShellArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitPowerShellArgs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"-NoProfile -EncodedCommand QUJD", []string{"-NoProfile", "-EncodedCommand", "QUJD"}},
		// Surrounding quotes are stripped so PowerShell never sees literal quote bytes.
		{"-EncodedCommand 'QUJD'", []string{"-EncodedCommand", "QUJD"}},
		{`-Command "Write-Host hello world"`, []string{"-Command", "Write-Host hello world"}},
		// Single quotes inside a double-quoted arg are preserved.
		{`-Command "Write-Host 'hello world'"`, []string{"-Command", "Write-Host 'hello world'"}},
		// Repeated spaces don't create empty args.
		{"a  b", []string{"a", "b"}},
		{"", nil},
	}
	for _, tc := range tests {
		got := splitPowerShellArgs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitPowerShellArgs(%q) = %#v, want %#v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitPowerShellArgs(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestEncodeUTF16Base64(t *testing.T) {
	// "AB" is 0x41 0x00 0x42 0x00 in UTF-16LE.
	if got := encodeUTF16Base64("AB"); got != "QQBCAA==" {
		t.Errorf("encodeUTF16Base64(\"AB\") = %q, want \"QQBCAA==\"", got)
	}
	// Round-trip a longer string through a decoder.
	in := "Write-Host 'hello'"
	if got := decodeUTF16Base64(t, encodeUTF16Base64(in)); got != in {
		t.Errorf("round-trip = %q, want %q", got, in)
	}
}

// decodeUTF16Base64 reverses encodeUTF16Base64 for assertions.
func decodeUTF16Base64(t *testing.T, s string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("odd UTF-16 byte length %d", len(raw))
	}
	var sb strings.Builder
	for i := 0; i < len(raw); i += 2 {
		sb.WriteRune(rune(uint16(raw[i]) | uint16(raw[i+1])<<8))
	}
	return sb.String()
}

// ─────────────────────────────────────────────────────────
// Command construction
// ─────────────────────────────────────────────────────────

func TestBuildCommandUnix(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		cmdStr       string
		become       bool
		becomeUser   string
		becomeMethod string
		wantArgs     []string
	}{
		{
			name:     "plain",
			cmdStr:   "echo hi",
			wantArgs: []string{"sh", "-c", "echo hi"},
		},
		{
			name:     "become defaults to sudo root",
			cmdStr:   "echo hi",
			become:   true,
			wantArgs: []string{"sh", "-c", "sudo -n -u 'root' -- sh -c 'echo hi'"},
		},
		{
			name:         "become su with user",
			cmdStr:       "echo hi",
			become:       true,
			becomeUser:   "bob",
			becomeMethod: "su",
			wantArgs:     []string{"sh", "-c", "su - 'bob' -c 'echo hi'"},
		},
		{
			name:         "unknown become method falls back to sudo",
			cmdStr:       "id",
			become:       true,
			becomeUser:   "alice",
			becomeMethod: "doas",
			wantArgs:     []string{"sh", "-c", "sudo -n -u 'alice' -- sh -c 'id'"},
		},
		{
			name:     "single quotes in command are escaped",
			cmdStr:   "echo 'x'",
			become:   true,
			wantArgs: []string{"sh", "-c", `sudo -n -u 'root' -- sh -c 'echo '\''x'\'''`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := buildCommandUnix(ctx, tc.cmdStr, tc.become, tc.becomeUser, tc.becomeMethod)
			if len(cmd.Args) != len(tc.wantArgs) {
				t.Fatalf("args = %#v, want %#v", cmd.Args, tc.wantArgs)
			}
			for i := range cmd.Args {
				if cmd.Args[i] != tc.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, cmd.Args[i], tc.wantArgs[i])
				}
			}
		})
	}
}

func TestBuildCommandWindowsPlain(t *testing.T) {
	cmd := buildCommandWindows(context.Background(), "ipconfig /all", false, "")
	want := []string{"cmd", "/c", "ipconfig /all"}
	assertArgs(t, cmd.Args, want)
}

func TestBuildCommandWindowsStripsShWrapper(t *testing.T) {
	// Ansible wraps Windows commands in /bin/sh -c '...'; the agent must
	// unwrap it, undo the POSIX quote escaping, and drop the "; sleep 0".
	in := `/bin/sh -c 'echo '"'"'hi'"'"' ; sleep 0'`
	cmd := buildCommandWindows(context.Background(), in, false, "")
	assertArgs(t, cmd.Args, []string{"cmd", "/c", "echo 'hi'"})
}

func TestBuildCommandWindowsPowerShellPassthrough(t *testing.T) {
	// PowerShell invocations run directly instead of being wrapped in
	// another powershell -Command layer.
	in := "powershell -NoProfile -EncodedCommand QUJDRA=="
	cmd := buildCommandWindows(context.Background(), in, false, "")
	assertArgs(t, cmd.Args, []string{"powershell", "-NoProfile", "-EncodedCommand", "QUJDRA=="})
}

func TestBuildCommandWindowsBecomeUsesScheduledTask(t *testing.T) {
	cmd := buildCommandWindows(context.Background(), "whoami", true, "alice")
	if cmd.Args[0] != "powershell" {
		t.Fatalf("args[0] = %q, want powershell", cmd.Args[0])
	}
	var encoded string
	for i, a := range cmd.Args {
		if a == "-EncodedCommand" && i+1 < len(cmd.Args) {
			encoded = cmd.Args[i+1]
		}
	}
	if encoded == "" {
		t.Fatalf("no -EncodedCommand in args %#v", cmd.Args)
	}
	script := decodeUTF16Base64(t, encoded)
	if !strings.Contains(script, "New-ScheduledTaskPrincipal -UserId 'alice'") {
		t.Errorf("script does not target user alice:\n%s", script)
	}
	if !strings.Contains(script, "Unregister-ScheduledTask") {
		t.Error("script does not clean up the scheduled task")
	}
}

// TestBuildCommandWindowsBecomeSystemRunsDirect covers the become=true cases
// that still run directly because the agent is already SYSTEM.
func TestBuildCommandWindowsBecomeSystemRunsDirect(t *testing.T) {
	for _, user := range []string{"", "SYSTEM", "Administrator"} {
		cmd := buildCommandWindows(context.Background(), "whoami", true, user)
		if cmd.Args[0] != "cmd" {
			t.Errorf("become user %q: args[0] = %q, want cmd (direct execution)", user, cmd.Args[0])
		}
	}
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ─────────────────────────────────────────────────────────
// Script command construction
// ─────────────────────────────────────────────────────────

func TestBuildScriptCommandUnixDefaults(t *testing.T) {
	skipOnWindows(t)
	script := []byte("#!/bin/sh\necho hi\n")
	cmd, cleanup, err := buildScriptCommand(context.Background(), script, "", false, "", "")
	if err != nil {
		t.Fatalf("buildScriptCommand: %v", err)
	}
	defer cleanup()

	if len(cmd.Args) != 1 {
		t.Fatalf("args = %#v, want a single direct-exec path", cmd.Args)
	}
	path := cmd.Args[0]
	if filepath.Ext(path) != ".sh" {
		t.Errorf("temp script %q lacks default .sh extension", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat temp script: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("temp script mode = %o, want 0700", perm)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(script) {
		t.Errorf("temp script content = %q err=%v, want %q", b, err, script)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cleanup did not remove the temp script")
	}
}

func TestBuildScriptCommandUnixPreservesExtension(t *testing.T) {
	skipOnWindows(t)
	cmd, cleanup, err := buildScriptCommand(context.Background(), []byte("print('hi')\n"), "job.py", false, "", "")
	if err != nil {
		t.Fatalf("buildScriptCommand: %v", err)
	}
	defer cleanup()
	if filepath.Ext(cmd.Args[0]) != ".py" {
		t.Errorf("temp script %q should keep the .py extension for shebang/interp dispatch", cmd.Args[0])
	}
}

func TestBuildScriptCommandUnixBecomeSudo(t *testing.T) {
	skipOnWindows(t)
	cmd, cleanup, err := buildScriptCommand(context.Background(), []byte("echo hi\n"), "x.sh", true, "", "")
	if err != nil {
		t.Fatalf("buildScriptCommand: %v", err)
	}
	defer cleanup()
	if len(cmd.Args) != 3 || cmd.Args[0] != "sh" || cmd.Args[1] != "-c" {
		t.Fatalf("args = %#v, want sh -c wrapper", cmd.Args)
	}
	if !strings.HasPrefix(cmd.Args[2], "sudo -n -u 'root' -- ") {
		t.Errorf("become script command = %q, want sudo -n -u 'root' -- prefix", cmd.Args[2])
	}
}

func TestBuildScriptCommandUnixBecomeSu(t *testing.T) {
	skipOnWindows(t)
	cmd, cleanup, err := buildScriptCommand(context.Background(), []byte("echo hi\n"), "x.sh", true, "bob", "su")
	if err != nil {
		t.Fatalf("buildScriptCommand: %v", err)
	}
	defer cleanup()
	if !strings.HasPrefix(cmd.Args[2], "su - 'bob' -c ") {
		t.Errorf("become-su script command = %q, want su - 'bob' -c prefix", cmd.Args[2])
	}
}

// ─────────────────────────────────────────────────────────
// handleExecRequest
// ─────────────────────────────────────────────────────────

func execResponse(t *testing.T, cs *captureStream) *pb.ExecResponse {
	t.Helper()
	msgs := cs.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	resp := msgs[0].GetExecResponse()
	if resp == nil {
		t.Fatalf("expected ExecResponse, got %T", msgs[0].Payload)
	}
	return resp
}

func TestHandleExecRequestSuccess(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId: "exec-ok",
		AgentId:   a.agentID,
		Command:   "echo hello",
	})

	resp := execResponse(t, cs)
	if !resp.Success || resp.Rc != 0 {
		t.Fatalf("success=%v rc=%d error=%q, want success rc=0", resp.Success, resp.Rc, resp.Error)
	}
	if got := string(resp.Stdout); got != "hello\n" {
		t.Errorf("stdout = %q, want \"hello\\n\"", got)
	}
	if resp.StartedAt == nil || resp.FinishedAt == nil {
		t.Error("StartedAt/FinishedAt not set")
	}
	if resp.Hostname != "test-host" || resp.AgentId != "agent-test" {
		t.Errorf("identity fields = %q/%q", resp.Hostname, resp.AgentId)
	}
}

func TestHandleExecRequestNonZeroExit(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId: "exec-rc",
		AgentId:   a.agentID,
		Command:   "echo oops >&2; exit 3",
	})

	resp := execResponse(t, cs)
	// A command that ran but exited non-zero is still Success=true.
	if !resp.Success {
		t.Fatalf("non-zero exit must keep Success=true, error=%q", resp.Error)
	}
	if resp.Rc != 3 {
		t.Errorf("rc = %d, want 3", resp.Rc)
	}
	if got := string(resp.Stderr); got != "oops\n" {
		t.Errorf("stderr = %q, want \"oops\\n\"", got)
	}
}

func TestHandleExecRequestDisabled(t *testing.T) {
	a, cs := newExecAgent(t)
	a.cfg.ExecEnabled = false

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId: "exec-disabled",
		AgentId:   a.agentID,
		Command:   "id",
	})

	resp := execResponse(t, cs)
	if resp.Success {
		t.Fatal("exec must fail when ExecEnabled=false")
	}
	if !strings.Contains(resp.Error, "remote execution is disabled") {
		t.Errorf("error = %q, want disabled message", resp.Error)
	}
}

func TestHandleExecRequestStdin(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId: "exec-stdin",
		AgentId:   a.agentID,
		Command:   "cat",
		Stdin:     []byte("piped input"),
	})

	resp := execResponse(t, cs)
	if !resp.Success || resp.Rc != 0 {
		t.Fatalf("success=%v rc=%d error=%q", resp.Success, resp.Rc, resp.Error)
	}
	if got := string(resp.Stdout); got != "piped input" {
		t.Errorf("stdout = %q, want stdin echoed back", got)
	}
}

func TestHandleExecRequestEnvironment(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId:   "exec-env",
		AgentId:     a.agentID,
		Command:     `printf '%s' "$DIRQ_TEST_VAR"`,
		Environment: map[string]string{"DIRQ_TEST_VAR": "injected"},
	})

	resp := execResponse(t, cs)
	if !resp.Success || resp.Rc != 0 {
		t.Fatalf("success=%v rc=%d error=%q", resp.Success, resp.Rc, resp.Error)
	}
	if got := string(resp.Stdout); got != "injected" {
		t.Errorf("stdout = %q, want env var value", got)
	}
}

func TestHandleExecRequestScript(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId:  "exec-script",
		AgentId:    a.agentID,
		Script:     []byte("#!/bin/sh\necho from-script\n"),
		ScriptName: "hello.sh",
	})

	resp := execResponse(t, cs)
	if !resp.Success || resp.Rc != 0 {
		t.Fatalf("success=%v rc=%d error=%q stderr=%q", resp.Success, resp.Rc, resp.Error, resp.Stderr)
	}
	if got := string(resp.Stdout); got != "from-script\n" {
		t.Errorf("stdout = %q, want \"from-script\\n\"", got)
	}
}

func TestHandleExecRequestTimeout(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)

	start := time.Now()
	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId:      "exec-timeout",
		AgentId:        a.agentID,
		Command:        "sleep 30",
		TimeoutSeconds: 1,
	})
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("exec was not killed by its 1s timeout (took %v)", elapsed)
	}
	resp := execResponse(t, cs)
	// A killed process surfaces as an ExitError with code -1: the command
	// "ran", so Success stays true and Rc records the abnormal exit.
	if resp.Rc != -1 {
		t.Errorf("rc = %d, want -1 for a timed-out (signal-killed) command", resp.Rc)
	}
}

// ─────────────────────────────────────────────────────────
// handlePutFile / handleFetchFile
// ─────────────────────────────────────────────────────────

func fileChunk(t *testing.T, cs *captureStream) *pb.FileChunk {
	t.Helper()
	msgs := cs.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	c := msgs[0].GetFileChunk()
	if c == nil {
		t.Fatalf("expected FileChunk, got %T", msgs[0].Payload)
	}
	return c
}

func TestHandlePutFileSuccess(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)
	dest := filepath.Join(t.TempDir(), "sub", "dir", "app.conf")

	a.handlePutFile(context.Background(), &pb.PutFileRequest{
		RequestId: "put-ok",
		AgentId:   a.agentID,
		DestPath:  dest,
		Content:   []byte("config data"),
		Mode:      0640,
	})

	ack := fileChunk(t, cs)
	if !ack.Success {
		t.Fatalf("put_file failed: %s", ack.Error)
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "config data" {
		t.Fatalf("content = %q err=%v", b, err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0640 {
		t.Errorf("mode = %o, want 0640", perm)
	}
}

func TestHandlePutFileRelativePathRejected(t *testing.T) {
	a, cs := newExecAgent(t)

	a.handlePutFile(context.Background(), &pb.PutFileRequest{
		RequestId: "put-rel",
		AgentId:   a.agentID,
		DestPath:  "relative/path.conf",
		Content:   []byte("x"),
	})

	ack := fileChunk(t, cs)
	if ack.Success {
		t.Fatal("relative dest_path must be rejected")
	}
	if !strings.Contains(ack.Error, "must be absolute") {
		t.Errorf("error = %q, want absolute-path message", ack.Error)
	}
}

func TestHandlePutFileTooLarge(t *testing.T) {
	a, cs := newExecAgent(t)
	dest := filepath.Join(t.TempDir(), "big.bin")

	a.handlePutFile(context.Background(), &pb.PutFileRequest{
		RequestId: "put-big",
		AgentId:   a.agentID,
		DestPath:  dest,
		Content:   make([]byte, maxFileSize+1),
	})

	ack := fileChunk(t, cs)
	if ack.Success {
		t.Fatal("oversized content must be rejected")
	}
	if !strings.Contains(ack.Error, "exceeds maximum size") {
		t.Errorf("error = %q, want size-limit message", ack.Error)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("oversized put_file must not create the file")
	}
}

func fetchResponse(t *testing.T, cs *captureStream) *pb.FetchFileResponse {
	t.Helper()
	msgs := cs.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	r := msgs[0].GetFetchResponse()
	if r == nil {
		t.Fatalf("expected FetchFileResponse, got %T", msgs[0].Payload)
	}
	return r
}

func TestHandleFetchFileSuccess(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)
	src := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(src, []byte("file body"), 0641); err != nil {
		t.Fatal(err)
	}

	a.handleFetchFile(context.Background(), &pb.FetchFileRequest{
		RequestId: "fetch-ok",
		AgentId:   a.agentID,
		SrcPath:   src,
	})

	resp := fetchResponse(t, cs)
	if !resp.Success {
		t.Fatalf("fetch failed: %s", resp.Error)
	}
	if string(resp.Content) != "file body" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Size != int64(len("file body")) {
		t.Errorf("size = %d, want %d", resp.Size, len("file body"))
	}
	if resp.Mode != 0641 {
		t.Errorf("mode = %o, want 0641", resp.Mode)
	}
}

func TestHandleFetchFileMissing(t *testing.T) {
	a, cs := newExecAgent(t)

	a.handleFetchFile(context.Background(), &pb.FetchFileRequest{
		RequestId: "fetch-missing",
		AgentId:   a.agentID,
		SrcPath:   filepath.Join(t.TempDir(), "nope"),
	})

	resp := fetchResponse(t, cs)
	if resp.Success {
		t.Fatal("fetching a missing file must fail")
	}
	if resp.Error == "" || len(resp.Content) != 0 {
		t.Errorf("error=%q content=%q", resp.Error, resp.Content)
	}
}

func TestHandleFetchFileRelativePathRejected(t *testing.T) {
	a, cs := newExecAgent(t)

	a.handleFetchFile(context.Background(), &pb.FetchFileRequest{
		RequestId: "fetch-rel",
		AgentId:   a.agentID,
		SrcPath:   "etc/passwd",
	})

	resp := fetchResponse(t, cs)
	if resp.Success {
		t.Fatal("relative src_path must be rejected")
	}
	if !strings.Contains(resp.Error, "must be absolute") {
		t.Errorf("error = %q, want absolute-path message", resp.Error)
	}
}

func TestHandleFetchFileTooLarge(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)
	src := filepath.Join(t.TempDir(), "huge.bin")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse file: over the limit without writing 100 MB.
	if err := f.Truncate(maxFileSize + 1); err != nil {
		f.Close()
		t.Skipf("cannot create sparse file: %v", err)
	}
	f.Close()

	a.handleFetchFile(context.Background(), &pb.FetchFileRequest{
		RequestId: "fetch-huge",
		AgentId:   a.agentID,
		SrcPath:   src,
	})

	resp := fetchResponse(t, cs)
	if resp.Success {
		t.Fatal("oversized file must be rejected")
	}
	if !strings.Contains(resp.Error, "exceeds maximum") {
		t.Errorf("error = %q, want size-limit message", resp.Error)
	}
}

// ─────────────────────────────────────────────────────────
// handleDeploy
// ─────────────────────────────────────────────────────────

func deployResponse(t *testing.T, cs *captureStream) *pb.DeployResponse {
	t.Helper()
	msgs := cs.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	r := msgs[0].GetDeployResponse()
	if r == nil {
		t.Fatalf("expected DeployResponse, got %T", msgs[0].Payload)
	}
	return r
}

func TestHandleDeploySuccess(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)
	dest := filepath.Join(t.TempDir(), "pkgs", "app.pkg")

	// The install command reads the package back, proving it was on disk
	// with the right content when the install ran.
	a.handleDeploy(context.Background(), &pb.DeployRequest{
		RequestId:      "deploy-ok",
		DestPath:       dest,
		Content:        []byte("PACKAGE-BYTES"),
		InstallCommand: "cat " + dest,
	})

	resp := deployResponse(t, cs)
	if !resp.Success || resp.Rc != 0 {
		t.Fatalf("success=%v rc=%d phase=%q error=%q", resp.Success, resp.Rc, resp.Phase, resp.Error)
	}
	if resp.Phase != "install" {
		t.Errorf("phase = %q, want \"install\"", resp.Phase)
	}
	if got := string(resp.Stdout); got != "PACKAGE-BYTES" {
		t.Errorf("install stdout = %q, want package content", got)
	}
	// The package file is removed after install, success or not.
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("deploy must remove the package file after install")
	}
}

func TestHandleDeployAppliesMode(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)
	dest := filepath.Join(t.TempDir(), "tool.bin")

	a.handleDeploy(context.Background(), &pb.DeployRequest{
		RequestId:      "deploy-mode",
		DestPath:       dest,
		Content:        []byte("#!/bin/sh\nexit 0\n"),
		Mode:           0755,
		InstallCommand: "test -x " + dest,
	})

	resp := deployResponse(t, cs)
	if !resp.Success || resp.Rc != 0 {
		t.Fatalf("mode 0755 not applied before install: rc=%d error=%q", resp.Rc, resp.Error)
	}
}

func TestHandleDeployInstallFailure(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)
	dest := filepath.Join(t.TempDir(), "bad.pkg")

	a.handleDeploy(context.Background(), &pb.DeployRequest{
		RequestId:      "deploy-fail",
		DestPath:       dest,
		Content:        []byte("PKG"),
		InstallCommand: "echo broken >&2; exit 7",
	})

	resp := deployResponse(t, cs)
	if resp.Success {
		t.Fatal("failed install must report Success=false")
	}
	if resp.Rc != 7 {
		t.Errorf("rc = %d, want 7", resp.Rc)
	}
	if resp.Error != "install exited with rc=7" {
		t.Errorf("error = %q, want \"install exited with rc=7\"", resp.Error)
	}
	if got := string(resp.Stderr); got != "broken\n" {
		t.Errorf("stderr = %q", got)
	}
	// Cleanup happens even when install fails.
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("deploy must remove the package file after a failed install")
	}
}

func TestHandleDeployRelativePathRejected(t *testing.T) {
	a, cs := newExecAgent(t)

	a.handleDeploy(context.Background(), &pb.DeployRequest{
		RequestId:      "deploy-rel",
		DestPath:       "relative/pkg.rpm",
		Content:        []byte("PKG"),
		InstallCommand: "true",
	})

	resp := deployResponse(t, cs)
	if resp.Success {
		t.Fatal("relative dest_path must be rejected")
	}
	if resp.Phase != "write" {
		t.Errorf("phase = %q, want \"write\"", resp.Phase)
	}
	if !strings.Contains(resp.Error, "must be absolute") {
		t.Errorf("error = %q, want absolute-path message", resp.Error)
	}
}

func TestHandleDeployDisabled(t *testing.T) {
	a, cs := newExecAgent(t)
	a.cfg.ExecEnabled = false

	a.handleDeploy(context.Background(), &pb.DeployRequest{
		RequestId:      "deploy-disabled",
		DestPath:       filepath.Join(t.TempDir(), "pkg"),
		Content:        []byte("PKG"),
		InstallCommand: "true",
	})

	resp := deployResponse(t, cs)
	if resp.Success {
		t.Fatal("deploy must fail when ExecEnabled=false")
	}
	if !strings.Contains(resp.Error, "remote execution is disabled") {
		t.Errorf("error = %q", resp.Error)
	}
}

func TestHandleDeployMkdirFailure(t *testing.T) {
	skipOnWindows(t)
	a, cs := newExecAgent(t)
	// Parent "directory" is a regular file, so MkdirAll must fail.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	a.handleDeploy(context.Background(), &pb.DeployRequest{
		RequestId:      "deploy-mkdir",
		DestPath:       filepath.Join(blocker, "pkg.rpm"),
		Content:        []byte("PKG"),
		InstallCommand: "true",
	})

	resp := deployResponse(t, cs)
	if resp.Success {
		t.Fatal("deploy into a non-directory must fail")
	}
	if resp.Phase != "write" {
		t.Errorf("phase = %q, want \"write\"", resp.Phase)
	}
	if !strings.Contains(resp.Error, "mkdir failed") {
		t.Errorf("error = %q, want mkdir failure", resp.Error)
	}
}
