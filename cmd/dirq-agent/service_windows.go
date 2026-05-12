//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/atgreen/dirq/internal/agent"
)

const serviceName = "DirQAgent"

// dirqService implements svc.Handler for the Windows Service Control Manager.
type dirqService struct {
	log *slog.Logger
	cfg agent.Config
}

func (s *dirqService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ag := agent.New(s.cfg, s.log)

	errCh := make(chan error, 1)
	go func() {
		errCh <- ag.Run(ctx)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-errCh:
			if err != nil {
				s.log.Error("agent error", "error", err)
				return false, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.log.Info("service stop requested")
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				return false, 0
			}
		}
	}
}

// runService starts the agent as a Windows Service if invoked by the SCM,
// or falls back to running in the foreground.
func runService(log *slog.Logger, cfg agent.Config) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("failed to detect service mode: %w", err)
	}

	if isService {
		log.Info("starting as Windows Service")
		return svc.Run(serviceName, &dirqService{log: log, cfg: cfg})
	}

	// Not running as a service — check for install/uninstall commands.
	if len(os.Args) > 1 {
		switch strings.ToLower(os.Args[1]) {
		case "install":
			return installService()
		case "uninstall", "remove":
			return removeService()
		}
	}

	return nil // signal to caller: run in foreground
}

func installService() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: "DirQ Agent",
		Description: "DirQ real-time endpoint query and execution agent",
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	fmt.Println("Service installed successfully. Start with: sc start DirQAgent")
	return nil
}

func removeService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}

	fmt.Println("Service removed successfully.")
	return nil
}
