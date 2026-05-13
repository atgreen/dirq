// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"fmt"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

func (s *Server) signServerMessage(msg *pb.ServerMessage) error {
	if s.signer == nil {
		return fmt.Errorf("server signer is not initialized")
	}
	return s.signer.SignServerMessage(msg, 5*time.Minute)
}
