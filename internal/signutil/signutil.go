// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package signutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/atgreen/dirq/internal/config"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

const (
	autoGenDir      = "/tmp/dirq-autosign"
	defaultValidity = 5 * time.Minute
)

type Config struct {
	PrivateKeyFile string
	PublicKeyFile  string
}

type Signer struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	keyID      string
}

type Verifier struct {
	publicKey ed25519.PublicKey
	keyID     string
}

func ConfigFromEnv(fileCfg ...*config.File) Config {
	var fc *config.File
	if len(fileCfg) > 0 {
		fc = fileCfg[0]
	}
	return Config{
		PrivateKeyFile: config.EnvOr("DIRQ_SIGNING_KEY", fc, "signing_key", ""),
		PublicKeyFile:  config.EnvOr("DIRQ_SIGNING_PUB", fc, "signing_pub", ""),
	}
}

func EnsureServerSigner(cfg Config, log *slog.Logger) (*Signer, error) {
	if cfg.PrivateKeyFile != "" && cfg.PublicKeyFile != "" {
		return LoadSigner(cfg)
	}

	if err := os.MkdirAll(autoGenDir, 0700); err != nil {
		return nil, fmt.Errorf("create auto-sign dir: %w", err)
	}
	cfg.PrivateKeyFile = filepath.Join(autoGenDir, "server_signing.key")
	cfg.PublicKeyFile = filepath.Join(autoGenDir, "server_signing.pub")

	if _, err := os.Stat(cfg.PrivateKeyFile); err == nil {
		return LoadSigner(cfg)
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	if err := os.WriteFile(cfg.PrivateKeyFile, []byte(base64.StdEncoding.EncodeToString(privateKey)), 0600); err != nil {
		return nil, fmt.Errorf("write signing key: %w", err)
	}
	if err := os.WriteFile(cfg.PublicKeyFile, []byte(base64.StdEncoding.EncodeToString(publicKey)), 0644); err != nil {
		return nil, fmt.Errorf("write signing pubkey: %w", err)
	}
	if log != nil {
		log.Warn("No signing key configured — auto-generating Ed25519 signing key. Set DIRQ_SIGNING_KEY and DIRQ_SIGNING_PUB for production use.")
	}
	return newSigner(privateKey)
}

func LoadSigner(cfg Config) (*Signer, error) {
	if cfg.PrivateKeyFile == "" || cfg.PublicKeyFile == "" {
		return nil, fmt.Errorf("both signing key and public key paths are required")
	}
	privateKeyData, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	privateKey, err := decodePrivateKey(privateKeyData)
	if err != nil {
		return nil, err
	}
	return newSigner(privateKey)
}

func newSigner(privateKey ed25519.PrivateKey) (*Signer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid ed25519 private key length: %d", len(privateKey))
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Signer{
		privateKey: privateKey,
		publicKey:  publicKey,
		keyID:      keyID(publicKey),
	}, nil
}

func NewVerifier(publicKey []byte, expectedKeyID string) (*Verifier, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid ed25519 public key length: %d", len(publicKey))
	}
	if expectedKeyID == "" {
		expectedKeyID = keyID(publicKey)
	}
	return &Verifier{publicKey: ed25519.PublicKey(publicKey), keyID: expectedKeyID}, nil
}

func (s *Signer) PublicKey() []byte {
	return append([]byte(nil), s.publicKey...)
}

func (s *Signer) KeyID() string {
	return s.keyID
}

func (s *Signer) SignServerMessage(msg *pb.ServerMessage, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = defaultValidity
	}
	now := time.Now().Unix()
	msg.SignerKeyId = s.keyID
	msg.SignedAtUnix = now
	msg.ExpiresAtUnix = now + int64(ttl.Seconds())
	msg.Signature = nil

	payload, err := canonicalMessageBytes(msg)
	if err != nil {
		return err
	}
	msg.Signature = ed25519.Sign(s.privateKey, payload)
	return nil
}

func (v *Verifier) VerifyServerMessage(msg *pb.ServerMessage, now time.Time) error {
	if msg.GetSignature() == nil {
		return fmt.Errorf("missing signature")
	}
	if msg.GetSignerKeyId() == "" || msg.GetSignerKeyId() != v.keyID {
		return fmt.Errorf("unexpected signer key id: %q", msg.GetSignerKeyId())
	}
	if msg.GetSignedAtUnix() == 0 || msg.GetExpiresAtUnix() == 0 {
		return fmt.Errorf("missing signature timestamps")
	}
	nowUnix := now.Unix()
	if nowUnix > msg.GetExpiresAtUnix() {
		return fmt.Errorf("signature expired")
	}
	if msg.GetSignedAtUnix() > nowUnix+30 {
		return fmt.Errorf("signature timestamp is in the future")
	}

	payload, err := canonicalMessageBytes(msg)
	if err != nil {
		return err
	}
	if !ed25519.Verify(v.publicKey, payload, msg.GetSignature()) {
		return fmt.Errorf("invalid signature")
	}
	return nil
}

// SignToken creates a session token for an agent ID: the base64 signature
// of the agent ID string. The token can be verified by any agent that has
// the server's signing public key.
func (s *Signer) SignToken(agentID string) string {
	sig := ed25519.Sign(s.privateKey, []byte(agentID))
	return base64.StdEncoding.EncodeToString(sig)
}

// VerifyToken checks that a session token was signed by this signer for the
// given agent ID. Returns true if valid.
func (s *Signer) VerifyToken(agentID, token string) bool {
	sig, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	return ed25519.Verify(s.privateKey.Public().(ed25519.PublicKey), []byte(agentID), sig)
}

// VerifyToken checks that a session token was signed by the server for the
// given agent ID. Returns true if valid.
func (v *Verifier) VerifyToken(agentID, token string) bool {
	sig, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	return ed25519.Verify(v.publicKey, []byte(agentID), sig)
}

func canonicalMessageBytes(msg *pb.ServerMessage) ([]byte, error) {
	cloned := proto.Clone(msg).(*pb.ServerMessage)
	cloned.Signature = nil
	return proto.MarshalOptions{Deterministic: true}.Marshal(cloned)
}

func decodePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(string(bytesTrimSpace(data)))
	if err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid ed25519 private key length: %d", len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

func bytesTrimSpace(data []byte) []byte {
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\n' || data[start] == '\r' || data[start] == '\t') {
		start++
	}
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\n' || data[end-1] == '\r' || data[end-1] == '\t') {
		end--
	}
	return data[start:end]
}

func keyID(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}
