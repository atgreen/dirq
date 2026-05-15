// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

const maxFileSize = 100 * 1024 * 1024 // 100 MB

// handleExecRequest runs a command on the agent and sends back an ExecResponse.
func (a *Agent) handleExecRequest(ctx context.Context, req *pb.ExecRequest) {
	hostname, _ := os.Hostname()

	a.log.Info("exec request received",
		slog.String("request_id", req.GetRequestId()),
		slog.String("command", req.GetCommand()),
		slog.Bool("become", req.GetBecome()),
	)

	if !a.cfg.ExecEnabled {
		a.log.Warn("exec request rejected: exec not enabled", slog.String("request_id", req.GetRequestId()))
		a.sendExecResponse(&pb.ExecResponse{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Error:     "remote execution is disabled on this agent",
		})
		return
	}

	// Set up timeout.
	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// If a script was provided, write it to a temp file and execute it.
	var cmd *exec.Cmd
	if len(req.GetScript()) > 0 {
		scriptCmd, cleanup, err := buildScriptCommand(execCtx, req.GetScript(), req.GetScriptName(), req.GetBecome(), req.GetBecomeUser(), req.GetBecomeMethod())
		if err != nil {
			a.sendExecResponse(&pb.ExecResponse{
				RequestId: req.GetRequestId(),
				AgentId:   a.agentID,
				Hostname:  hostname,
				Success:   false,
				Error:     "script setup failed: " + err.Error(),
				Rc:        -1,
			})
			return
		}
		defer cleanup()
		cmd = scriptCmd
	} else {
		cmd = buildCommand(execCtx, req.GetCommand(), req.GetBecome(), req.GetBecomeUser(), req.GetBecomeMethod())
	}

	// Set extra environment variables.
	if len(req.GetEnvironment()) > 0 {
		cmd.Env = os.Environ()
		for k, v := range req.GetEnvironment() {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Pipe stdin if provided. Ansible modules send Python code via stdin.
	if len(req.GetStdin()) > 0 {
		cmd.Stdin = bytes.NewReader(req.GetStdin())
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	startedAt := time.Now()
	err := cmd.Run()
	finishedAt := time.Now()

	resp := &pb.ExecResponse{
		RequestId:  req.GetRequestId(),
		AgentId:    a.agentID,
		Hostname:   hostname,
		Stdout:     stdoutBuf.Bytes(),
		Stderr:     stderrBuf.Bytes(),
		StartedAt:  timestamppb.New(startedAt),
		FinishedAt: timestamppb.New(finishedAt),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.Rc = int32(exitErr.ExitCode())
			resp.Success = true // command ran, just non-zero exit
		} else {
			resp.Success = false
			resp.Error = err.Error()
			resp.Rc = -1
		}
	} else {
		resp.Rc = 0
		resp.Success = true
	}

	a.log.Info("exec completed",
		slog.String("request_id", req.GetRequestId()),
		slog.Int("rc", int(resp.Rc)),
		slog.Bool("success", resp.Success),
		slog.Duration("duration", finishedAt.Sub(startedAt)),
	)

	a.sendExecResponse(resp)
}

// buildCommand constructs the os/exec.Cmd for the current platform.
func buildCommand(ctx context.Context, cmdStr string, become bool, becomeUser, becomeMethod string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return buildCommandWindows(ctx, cmdStr, become, becomeUser)
	}
	return buildCommandUnix(ctx, cmdStr, become, becomeUser, becomeMethod)
}

// buildCommandUnix builds a command for Linux/macOS.
func buildCommandUnix(ctx context.Context, cmdStr string, become bool, becomeUser, becomeMethod string) *exec.Cmd {
	if become {
		if becomeUser == "" {
			becomeUser = "root"
		}
		if becomeMethod == "" {
			becomeMethod = "sudo"
		}
		switch becomeMethod {
		case "sudo":
			cmdStr = fmt.Sprintf("sudo -n -u %s -- sh -c %s", shellQuote(becomeUser), shellQuote(cmdStr))
		case "su":
			cmdStr = fmt.Sprintf("su - %s -c %s", shellQuote(becomeUser), shellQuote(cmdStr))
		default:
			// Fall back to sudo for unknown methods.
			cmdStr = fmt.Sprintf("sudo -n -u %s -- sh -c %s", shellQuote(becomeUser), shellQuote(cmdStr))
		}
	}
	return exec.CommandContext(ctx, "sh", "-c", cmdStr)
}

// buildCommandWindows builds a command for Windows.
//
// On Windows the agent is expected to run as the SYSTEM account (via Windows
// Service) so it already has full administrative privileges. This means:
//
//   - become=false: run directly via cmd /c
//   - become=true, no user specified: run directly (agent is already SYSTEM)
//   - become=true, user specified: use PowerShell to run as that user
//
// Note: running as a different user on Windows without a password prompt
// requires the agent to run as SYSTEM, which has the SeAssignPrimaryTokenPrivilege
// needed to launch processes as other users. We use PowerShell's
// scheduled-task trick to run as another user without storing passwords.
func buildCommandWindows(ctx context.Context, cmdStr string, become bool, becomeUser string) *exec.Cmd {
	if !become || becomeUser == "" || becomeUser == "Administrator" || becomeUser == "SYSTEM" {
		// Detect PowerShell: if the command starts with powershell/pwsh or
		// contains PowerShell-specific syntax, use PowerShell directly.
		// Ansible sends PowerShell-wrapped commands when ansible_shell_type=powershell.
		lower := strings.ToLower(strings.TrimSpace(cmdStr))
		if strings.HasPrefix(lower, "powershell") || strings.HasPrefix(lower, "pwsh") {
			return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", cmdStr)
		}
		return exec.CommandContext(ctx, "cmd", "/c", cmdStr)
	}

	// Run as a different user using a one-shot scheduled task.
	// This avoids the interactive password prompt of runas.exe.
	// The SYSTEM account can create and run tasks as any local user.
	// Use a unique task name and output file per request to avoid
	// collisions and symlink attacks (#5, #10).
	taskID := fmt.Sprintf("DirQExec-%d", time.Now().UnixNano())
	outFile := filepath.Join(os.TempDir(), taskID+".txt")
	psScript := fmt.Sprintf(
		`$action = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument '/c %s > %s 2>&1'`+"\n"+
			`$principal = New-ScheduledTaskPrincipal -UserId '%s' -LogonType S4U -RunLevel Highest`+"\n"+
			`$task = New-ScheduledTask -Action $action -Principal $principal`+"\n"+
			`Register-ScheduledTask -TaskName '%s' -InputObject $task -Force | Out-Null`+"\n"+
			`Start-ScheduledTask -TaskName '%s'`+"\n"+
			// Wait for completion (up to timeout is handled by context).
			`do { Start-Sleep -Milliseconds 250; $info = Get-ScheduledTaskInfo -TaskName '%s' } while ($info.LastTaskResult -eq 267009)`+"\n"+
			`Unregister-ScheduledTask -TaskName '%s' -Confirm:$false`+"\n"+
			`Get-Content '%s'`+"\n"+
			`Remove-Item '%s' -Force -ErrorAction SilentlyContinue`,
		escapePowerShellArg(cmdStr),
		outFile,
		escapePowerShellArg(becomeUser),
		taskID, taskID, taskID, taskID,
		outFile, outFile,
	)

	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive",
		"-EncodedCommand", encodeUTF16Base64(psScript))
}

// handlePutFile writes content to a file on the agent.
func (a *Agent) handlePutFile(ctx context.Context, req *pb.PutFileRequest) {
	hostname, _ := os.Hostname()

	a.log.Info("put_file request received",
		slog.String("request_id", req.GetRequestId()),
		slog.String("dest_path", req.GetDestPath()),
		slog.Int("content_size", len(req.GetContent())),
	)

	if !a.cfg.ExecEnabled {
		a.log.Warn("put_file request rejected: exec not enabled", slog.String("request_id", req.GetRequestId()))
		a.sendFileChunk(&pb.FileChunk{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Error:     "remote execution is disabled on this agent",
		})
		return
	}

	destPath := filepath.Clean(req.GetDestPath())
	if !filepath.IsAbs(destPath) {
		a.sendFileChunk(&pb.FileChunk{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Error:     fmt.Sprintf("dest_path must be absolute, got: %s", req.GetDestPath()),
		})
		return
	}

	if len(req.GetContent()) > maxFileSize {
		a.sendFileChunk(&pb.FileChunk{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Error:     fmt.Sprintf("file content exceeds maximum size of %d bytes", maxFileSize),
		})
		return
	}

	var writeErr error

	if req.GetBecome() && runtime.GOOS != "windows" {
		// Linux: use sudo tee to write as another user.
		becomeUser := req.GetBecomeUser()
		if becomeUser == "" {
			becomeUser = "root"
		}
		cmdStr := fmt.Sprintf("sudo -n -u %s tee %s > /dev/null", shellQuote(becomeUser), shellQuote(destPath))
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		cmd.Stdin = bytes.NewReader(req.GetContent())
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		if err := cmd.Run(); err != nil {
			writeErr = fmt.Errorf("sudo tee failed: %s: %w", stderrBuf.String(), err)
		} else if req.GetMode() != 0 {
			chmodCmd := fmt.Sprintf("sudo -n chmod %04o %s", req.GetMode(), shellQuote(destPath))
			chCmd := exec.CommandContext(ctx, "sh", "-c", chmodCmd)
			if err := chCmd.Run(); err != nil {
				writeErr = fmt.Errorf("chmod failed: %w", err)
			}
		}
	} else {
		// Direct write. On Windows the agent runs as SYSTEM so it can write anywhere.
		mode := os.FileMode(0644)
		if req.GetMode() != 0 && runtime.GOOS != "windows" {
			mode = os.FileMode(req.GetMode())
		}
		// Ensure parent directory exists.
		dir := filepath.Dir(destPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			writeErr = fmt.Errorf("mkdir failed: %w", err)
		} else {
			writeErr = os.WriteFile(destPath, req.GetContent(), mode)
		}
	}

	ack := &pb.FileChunk{
		RequestId: req.GetRequestId(),
		AgentId:   a.agentID,
		Hostname:  hostname,
	}

	if writeErr != nil {
		ack.Success = false
		ack.Error = writeErr.Error()
		a.log.Error("put_file failed", slog.String("request_id", req.GetRequestId()), slog.String("error", writeErr.Error()))
	} else {
		ack.Success = true
		a.log.Info("put_file completed", slog.String("request_id", req.GetRequestId()), slog.String("dest_path", destPath))
	}

	a.sendFileChunk(ack)
}

// handleFetchFile reads a file from the agent and sends its content back.
func (a *Agent) handleFetchFile(ctx context.Context, req *pb.FetchFileRequest) {
	hostname, _ := os.Hostname()

	a.log.Info("fetch_file request received",
		slog.String("request_id", req.GetRequestId()),
		slog.String("src_path", req.GetSrcPath()),
	)

	if !a.cfg.ExecEnabled {
		a.log.Warn("fetch_file request rejected: exec not enabled", slog.String("request_id", req.GetRequestId()))
		a.sendFetchFileResponse(&pb.FetchFileResponse{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Error:     "remote execution is disabled on this agent",
		})
		return
	}

	srcPath := filepath.Clean(req.GetSrcPath())
	if !filepath.IsAbs(srcPath) {
		a.sendFetchFileResponse(&pb.FetchFileResponse{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Error:     fmt.Sprintf("src_path must be absolute, got: %s", req.GetSrcPath()),
		})
		return
	}

	var content []byte
	var fileMode int32
	var fileSize int64
	var readErr error

	if req.GetBecome() && runtime.GOOS != "windows" {
		// Linux: use sudo cat.
		becomeUser := req.GetBecomeUser()
		if becomeUser == "" {
			becomeUser = "root"
		}
		cmdStr := fmt.Sprintf("sudo -n -u %s cat %s", shellQuote(becomeUser), shellQuote(srcPath))
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		if err := cmd.Run(); err != nil {
			readErr = fmt.Errorf("sudo cat failed: %s: %w", stderrBuf.String(), err)
		} else {
			content = stdoutBuf.Bytes()
			fileSize = int64(len(content))
		}
	} else {
		// Direct read. On Windows the agent runs as SYSTEM so it can read anything.
		info, err := os.Stat(srcPath)
		if err != nil {
			readErr = err
		} else {
			fileSize = info.Size()
			if fileSize > maxFileSize {
				readErr = fmt.Errorf("file size %d exceeds maximum of %d bytes", fileSize, maxFileSize)
			} else {
				if runtime.GOOS != "windows" {
					fileMode = int32(info.Mode().Perm())
				}
				content, readErr = os.ReadFile(srcPath)
			}
		}
	}

	resp := &pb.FetchFileResponse{
		RequestId: req.GetRequestId(),
		AgentId:   a.agentID,
		Hostname:  hostname,
	}

	if readErr != nil {
		resp.Success = false
		resp.Error = readErr.Error()
		a.log.Error("fetch_file failed", slog.String("request_id", req.GetRequestId()), slog.String("error", readErr.Error()))
	} else {
		resp.Success = true
		resp.Content = content
		resp.Mode = fileMode
		resp.Size = fileSize
		a.log.Info("fetch_file completed", slog.String("request_id", req.GetRequestId()), slog.String("src_path", srcPath), slog.Int64("size", fileSize))
	}

	a.sendFetchFileResponse(resp)
}

// ─────────────────────────────────────────────────────────
// Broadcast deploy
// ─────────────────────────────────────────────────────────

// handleDeploy writes a package to disk, runs the install command, cleans up,
// and sends a DeployResponse. Used by the broadcast deploy path — the package
// binary travels through the mesh once (like a query) instead of once per host.
func (a *Agent) handleDeploy(ctx context.Context, req *pb.DeployRequest) {
	hostname, _ := os.Hostname()

	a.log.Info("deploy request received",
		slog.String("request_id", req.GetRequestId()),
		slog.String("dest_path", req.GetDestPath()),
		slog.Int("content_size", len(req.GetContent())),
		slog.String("install_command", req.GetInstallCommand()),
	)

	if !a.cfg.ExecEnabled {
		a.sendDeployResponse(&pb.DeployResponse{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Phase:     "write",
			Error:     "remote execution is disabled on this agent",
		})
		return
	}

	// Set up timeout.
	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	deployCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Phase 1: Write the package to disk.
	destPath := filepath.Clean(req.GetDestPath())
	if !filepath.IsAbs(destPath) {
		a.sendDeployResponse(&pb.DeployResponse{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Phase:     "write",
			Error:     fmt.Sprintf("dest_path must be absolute, got: %s", destPath),
		})
		return
	}

	mode := os.FileMode(0644)
	if req.GetMode() != 0 && runtime.GOOS != "windows" {
		mode = os.FileMode(req.GetMode())
	}
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		a.sendDeployResponse(&pb.DeployResponse{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Phase:     "write",
			Error:     fmt.Sprintf("mkdir failed: %v", err),
		})
		return
	}
	if err := os.WriteFile(destPath, req.GetContent(), mode); err != nil {
		a.sendDeployResponse(&pb.DeployResponse{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Phase:     "write",
			Error:     fmt.Sprintf("write failed: %v", err),
		})
		return
	}

	a.log.Info("deploy package written", slog.String("request_id", req.GetRequestId()), slog.String("dest_path", destPath))

	// Phase 2: Run the install command.
	cmd := buildCommand(deployCtx, req.GetInstallCommand(), req.GetBecome(), req.GetBecomeUser(), "")

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()

	// Clean up the temp file regardless of install outcome.
	os.Remove(destPath)

	resp := &pb.DeployResponse{
		RequestId: req.GetRequestId(),
		AgentId:   a.agentID,
		Hostname:  hostname,
		Phase:     "install",
		Stdout:    stdoutBuf.Bytes(),
		Stderr:    stderrBuf.Bytes(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.Rc = int32(exitErr.ExitCode())
			resp.Success = false
			resp.Error = fmt.Sprintf("install exited with rc=%d", exitErr.ExitCode())
		} else {
			resp.Success = false
			resp.Error = err.Error()
			resp.Rc = -1
		}
	} else {
		resp.Rc = 0
		resp.Success = true
	}

	a.log.Info("deploy completed",
		slog.String("request_id", req.GetRequestId()),
		slog.Int("rc", int(resp.Rc)),
		slog.Bool("success", resp.Success),
	)

	a.sendDeployResponse(resp)
}

// ─────────────────────────────────────────────────────────
// Script execution
// ─────────────────────────────────────────────────────────

// buildScriptCommand writes script content to a temp file and returns a
// command to execute it. The cleanup function removes the temp file.
//
// On Linux: preserves the original extension (for shebang dispatch), writes
// to /tmp, chmod +x, and runs it directly. If become is set, runs via sudo.
//
// On Windows: writes as .ps1, runs with PowerShell.
func buildScriptCommand(ctx context.Context, script []byte, scriptName string, become bool, becomeUser, becomeMethod string) (*exec.Cmd, func(), error) {
	ext := filepath.Ext(scriptName)

	if runtime.GOOS == "windows" {
		return buildScriptCommandWindows(ctx, script, ext, become, becomeUser)
	}
	return buildScriptCommandUnix(ctx, script, ext, become, becomeUser, becomeMethod)
}

func buildScriptCommandUnix(ctx context.Context, script []byte, ext string, become bool, becomeUser, becomeMethod string) (*exec.Cmd, func(), error) {
	if ext == "" {
		ext = ".sh"
	}

	tmpFile, err := os.CreateTemp("", "dirq-script-*"+ext)
	if err != nil {
		return nil, nil, fmt.Errorf("create temp script: %w", err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() { os.Remove(tmpPath) }

	if _, err := tmpFile.Write(script); err != nil {
		tmpFile.Close()
		cleanup()
		return nil, nil, fmt.Errorf("write script: %w", err)
	}
	tmpFile.Close()

	if err := os.Chmod(tmpPath, 0700); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("chmod script: %w", err)
	}

	if become {
		if becomeUser == "" {
			becomeUser = "root"
		}
		if becomeMethod == "" {
			becomeMethod = "sudo"
		}
		var cmdStr string
		switch becomeMethod {
		case "su":
			cmdStr = fmt.Sprintf("su - %s -c %s", shellQuote(becomeUser), shellQuote(tmpPath))
		default: // sudo
			cmdStr = fmt.Sprintf("sudo -n -u %s -- %s", shellQuote(becomeUser), tmpPath)
		}
		return exec.CommandContext(ctx, "sh", "-c", cmdStr), cleanup, nil
	}

	return exec.CommandContext(ctx, tmpPath), cleanup, nil
}

func buildScriptCommandWindows(ctx context.Context, script []byte, ext string, become bool, becomeUser string) (*exec.Cmd, func(), error) {
	if ext == "" || ext == ".sh" {
		// Default to PowerShell on Windows.
		ext = ".ps1"
	}

	tmpFile, err := os.CreateTemp("", "dirq-script-*"+ext)
	if err != nil {
		return nil, nil, fmt.Errorf("create temp script: %w", err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() { os.Remove(tmpPath) }

	if _, err := tmpFile.Write(script); err != nil {
		tmpFile.Close()
		cleanup()
		return nil, nil, fmt.Errorf("write script: %w", err)
	}
	tmpFile.Close()

	if ext == ".ps1" {
		return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", tmpPath), cleanup, nil
	}

	// For other extensions (.bat, .cmd), run via cmd.
	return exec.CommandContext(ctx, "cmd", "/c", tmpPath), cleanup, nil
}

// ─────────────────────────────────────────────────────────
// Stream senders
// ─────────────────────────────────────────────────────────

func (a *Agent) sendExecResponse(resp *pb.ExecResponse) {
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_ExecResponse{ExecResponse: resp},
	})
	if err != nil {
		a.log.Error("failed to send exec response", slog.String("request_id", resp.GetRequestId()), slog.String("error", err.Error()))
	}
}

func (a *Agent) sendFileChunk(chunk *pb.FileChunk) {
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_FileChunk{FileChunk: chunk},
	})
	if err != nil {
		a.log.Error("failed to send file chunk", slog.String("request_id", chunk.GetRequestId()), slog.String("error", err.Error()))
	}
}

func (a *Agent) sendFetchFileResponse(resp *pb.FetchFileResponse) {
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_FetchResponse{FetchResponse: resp},
	})
	if err != nil {
		a.log.Error("failed to send fetch file response", slog.String("request_id", resp.GetRequestId()), slog.String("error", err.Error()))
	}
}

func (a *Agent) sendDeployResponse(resp *pb.DeployResponse) {
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_DeployResponse{DeployResponse: resp},
	})
	if err != nil {
		a.log.Error("failed to send deploy response", slog.String("request_id", resp.GetRequestId()), slog.String("error", err.Error()))
	}
}

// ─────────────────────────────────────────────────────────
// Shell helpers
// ─────────────────────────────────────────────────────────

// shellQuote wraps a string in single quotes for safe POSIX shell use.
func shellQuote(s string) string {
	quoted := "'"
	for _, c := range s {
		if c == '\'' {
			quoted += "'\\''"
		} else {
			quoted += string(c)
		}
	}
	quoted += "'"
	return quoted
}

// escapePowerShellArg escapes a string for use inside a PowerShell -Command argument.
func escapePowerShellArg(s string) string {
	// Escape single quotes by doubling them (PowerShell convention).
	result := ""
	for _, c := range s {
		if c == '\'' {
			result += "''"
		} else if c == '"' {
			result += "`\""
		} else {
			result += string(c)
		}
	}
	return result
}

// encodeUTF16Base64 encodes a string as UTF-16LE base64 for PowerShell's
// -EncodedCommand flag. This avoids all shell metacharacter injection risks.
func encodeUTF16Base64(s string) string {
	var buf bytes.Buffer
	for _, r := range s {
		binary.Write(&buf, binary.LittleEndian, uint16(r))
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
