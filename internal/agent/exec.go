package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	// Check if exec is enabled.
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

	// Build the command.
	cmdStr := req.GetCommand()

	// Handle become (privilege escalation).
	if req.GetBecome() {
		becomeUser := req.GetBecomeUser()
		if becomeUser == "" {
			if runtime.GOOS == "windows" {
				becomeUser = "Administrator"
			} else {
				becomeUser = "root"
			}
		}

		if runtime.GOOS == "windows" {
			// On Windows, wrap with runas.
			cmdStr = fmt.Sprintf("runas /user:%s \"%s\"", becomeUser, cmdStr)
		} else {
			// On Linux/Unix, wrap with sudo -u <user>.
			cmdStr = fmt.Sprintf("sudo -u %s -- sh -c %s", becomeUser, shellQuote(cmdStr))
		}
	}

	// Build the os/exec command.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", cmdStr)
	} else {
		cmd = exec.Command("sh", "-c", cmdStr)
	}

	// Set extra environment variables.
	if len(req.GetEnvironment()) > 0 {
		cmd.Env = os.Environ()
		for k, v := range req.GetEnvironment() {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Set up timeout.
	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second // default 60s
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prevEnv := cmd.Env
	cmd = exec.CommandContext(execCtx, cmd.Path, cmd.Args[1:]...)
	cmd.Env = prevEnv

	// Apply extra environment if set.
	if len(req.GetEnvironment()) > 0 {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		for k, v := range req.GetEnvironment() {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
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

// handlePutFile writes content to a file on the agent.
func (a *Agent) handlePutFile(ctx context.Context, req *pb.PutFileRequest) {
	hostname, _ := os.Hostname()

	a.log.Info("put_file request received",
		slog.String("request_id", req.GetRequestId()),
		slog.String("dest_path", req.GetDestPath()),
		slog.Int("content_size", len(req.GetContent())),
	)

	// Check if exec is enabled (file operations require exec).
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

	// Validate dest_path is absolute.
	if !filepath.IsAbs(req.GetDestPath()) {
		a.sendFileChunk(&pb.FileChunk{
			RequestId: req.GetRequestId(),
			AgentId:   a.agentID,
			Hostname:  hostname,
			Success:   false,
			Error:     fmt.Sprintf("dest_path must be absolute, got: %s", req.GetDestPath()),
		})
		return
	}

	// Max file size check.
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
		// Use sudo tee to write the file.
		becomeUser := req.GetBecomeUser()
		if becomeUser == "" {
			becomeUser = "root"
		}
		cmdStr := fmt.Sprintf("sudo -u %s tee %s > /dev/null", becomeUser, shellQuote(req.GetDestPath()))
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		cmd.Stdin = bytes.NewReader(req.GetContent())
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		if err := cmd.Run(); err != nil {
			writeErr = fmt.Errorf("sudo tee failed: %s: %w", stderrBuf.String(), err)
		} else if req.GetMode() != 0 {
			// Set file mode via sudo chmod.
			chmodCmd := fmt.Sprintf("sudo -u %s chmod %04o %s", becomeUser, req.GetMode(), shellQuote(req.GetDestPath()))
			chCmd := exec.CommandContext(ctx, "sh", "-c", chmodCmd)
			if err := chCmd.Run(); err != nil {
				writeErr = fmt.Errorf("chmod failed: %w", err)
			}
		}
	} else {
		// Direct write.
		mode := os.FileMode(0644)
		if req.GetMode() != 0 && runtime.GOOS != "windows" {
			mode = os.FileMode(req.GetMode())
		}
		writeErr = os.WriteFile(req.GetDestPath(), req.GetContent(), mode)
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
		a.log.Info("put_file completed", slog.String("request_id", req.GetRequestId()), slog.String("dest_path", req.GetDestPath()))
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

	// Check if exec is enabled.
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

	// Validate src_path is absolute.
	if !filepath.IsAbs(req.GetSrcPath()) {
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
		// Use sudo cat to read the file.
		becomeUser := req.GetBecomeUser()
		if becomeUser == "" {
			becomeUser = "root"
		}
		cmdStr := fmt.Sprintf("sudo -u %s cat %s", becomeUser, shellQuote(req.GetSrcPath()))
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
		// Direct read.
		info, err := os.Stat(req.GetSrcPath())
		if err != nil {
			readErr = err
		} else {
			fileSize = info.Size()
			if fileSize > maxFileSize {
				readErr = fmt.Errorf("file size %d exceeds maximum of %d bytes", fileSize, maxFileSize)
			} else {
				fileMode = int32(info.Mode().Perm())
				content, readErr = os.ReadFile(req.GetSrcPath())
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
		a.log.Info("fetch_file completed", slog.String("request_id", req.GetRequestId()), slog.String("src_path", req.GetSrcPath()), slog.Int64("size", fileSize))
	}

	a.sendFetchFileResponse(resp)
}

// sendExecResponse sends an ExecResponse upstream.
func (a *Agent) sendExecResponse(resp *pb.ExecResponse) {
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_ExecResponse{
			ExecResponse: resp,
		},
	})
	if err != nil {
		a.log.Error("failed to send exec response", slog.String("request_id", resp.GetRequestId()), slog.String("error", err.Error()))
	}
}

// sendFileChunk sends a FileChunk (put file ack) upstream.
func (a *Agent) sendFileChunk(chunk *pb.FileChunk) {
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_FileChunk{
			FileChunk: chunk,
		},
	})
	if err != nil {
		a.log.Error("failed to send file chunk", slog.String("request_id", chunk.GetRequestId()), slog.String("error", err.Error()))
	}
}

// sendFetchFileResponse sends a FetchFileResponse upstream.
func (a *Agent) sendFetchFileResponse(resp *pb.FetchFileResponse) {
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_FetchResponse{
			FetchResponse: resp,
		},
	})
	if err != nil {
		a.log.Error("failed to send fetch file response", slog.String("request_id", resp.GetRequestId()), slog.String("error", err.Error()))
	}
}

// shellQuote wraps a string in single quotes for safe shell use.
func shellQuote(s string) string {
	// Replace single quotes with '\'' and wrap in single quotes.
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
