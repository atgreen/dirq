// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"fmt"
	"time"

	"github.com/atgreen/dirq/internal/signutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

func (a *Agent) setServerVerifier(publicKey []byte, keyID string) error {
	verifier, err := signutil.NewVerifier(publicKey, keyID)
	if err != nil {
		return err
	}
	a.serverVerifier = verifier
	return nil
}

func (a *Agent) verifyServerMessage(msg *pb.ServerMessage) error {
	if a.serverVerifier == nil {
		return fmt.Errorf("server verifier is not initialized")
	}
	return a.serverVerifier.VerifyServerMessage(msg, time.Now())
}
