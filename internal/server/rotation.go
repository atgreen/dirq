// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"fmt"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// broadcastRotation sends a RotateCommand through all connected zone leaders.
// The command is signed so agents can verify its authenticity.
// It returns the number of zone leaders the command was sent to.
func (s *Server) broadcastRotation(rotationType pb.RotateCommand_RotationType, reason string, staggerSeconds int32) (int, error) {
	cmd := &pb.RotateCommand{
		Type:           rotationType,
		Reason:         reason,
		StaggerSeconds: staggerSeconds,
	}

	// For signing key rotation, include the new key in the command so
	// agents can update their verifier without re-registering.
	if rotationType == pb.RotateCommand_ROTATION_TYPE_SIGNING_KEY {
		cmd.NewSigningPublicKey = s.signer.PublicKey()
		cmd.NewSigningKeyId = s.signer.KeyID()
		// Old keys would be populated from config if available.
	}

	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_RotateCommand{
			RotateCommand: cmd,
		},
	}
	if err := s.signServerMessage(msg); err != nil {
		return 0, fmt.Errorf("sign rotate command: %w", err)
	}

	sent := 0
	s.mu.RLock()
	for _, as := range s.streams {
		select {
		case as.send <- msg:
			sent++
		default:
			s.log.Warn("zone leader send buffer full during rotation", "agent_id", as.agentID)
		}
	}
	s.mu.RUnlock()

	s.log.Info("rotation command broadcast",
		"type", rotationType.String(),
		"reason", reason,
		"zone_leaders", sent,
	)
	return sent, nil
}
