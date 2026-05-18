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

// serverMessageVerifier is the interface satisfied by both *signutil.Verifier
// and *signutil.MultiVerifier. Keeping this interface lets the agent swap in
// a MultiVerifier during key rotation without changing all call sites.
type serverMessageVerifier interface {
	VerifyServerMessage(msg *pb.ServerMessage, now time.Time) error
	VerifyToken(agentID, token string) bool
}

func (a *Agent) setServerVerifier(publicKey []byte, keyID string, oldPublicKeys [][]byte) error {
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

	primary, err := signutil.NewVerifier(publicKey, keyID)
	if err != nil {
		return err
	}

	if len(oldPublicKeys) == 0 {
		a.serverVerifier = primary
		return nil
	}

	// Build a MultiVerifier so messages signed by old keys are also accepted
	// during a key rotation window.
	verifiers := []*signutil.Verifier{primary}
	for _, oldKey := range oldPublicKeys {
		v, err := signutil.NewVerifier(oldKey, "")
		if err != nil {
			a.log.Warn("skipping invalid old signing public key", "error", err)
			continue
		}
		verifiers = append(verifiers, v)
	}
	a.serverVerifier = signutil.NewMultiVerifier(verifiers...)
	return nil
}

func (a *Agent) verifyServerMessage(msg *pb.ServerMessage) error {
	if a.serverVerifier == nil {
		return fmt.Errorf("server verifier is not initialized")
	}
	return a.serverVerifier.VerifyServerMessage(msg, time.Now())
}
