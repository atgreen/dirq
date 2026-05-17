// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/atgreen/dirq/internal/signutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

func (a *Agent) setServerVerifier(publicKey []byte, keyID string) error {
	// If a signing public key is pinned in the config, verify the server's
	// key matches before trusting it. This prevents MITM at registration.
	if a.cfg.FileCfg != nil {
		if pinned := a.cfg.FileCfg.Get("signing_public_key"); pinned != "" {
			pinnedKey, err := base64.StdEncoding.DecodeString(pinned)
			if err != nil {
				return fmt.Errorf("invalid signing_public_key in config: %w", err)
			}
			if subtle.ConstantTimeCompare(pinnedKey, publicKey) != 1 {
				return fmt.Errorf("server signing key does not match pinned key in agent.conf — possible MITM")
			}
			a.log.Info("server signing key matches pinned key")
		}
	}

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
