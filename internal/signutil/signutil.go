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
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/atgreen/dirq/internal/config"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// autoGenDir returns the directory for auto-generated signing keys.
func autoGenDir() string {
	return filepath.Join(config.DataDir(), "signing")
}

const (
	defaultValidity = 5 * time.Minute
)

type Config struct {
	PrivateKeyFile    string
	PublicKeyFile     string
	OldPrivateKeyFile string // previous signing key (trusted during rotation)
	OldPublicKeyFile  string // previous signing public key
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
		PrivateKeyFile:    config.EnvOr("DIRQ_SIGNING_KEY", fc, "signing_key", ""),
		PublicKeyFile:     config.EnvOr("DIRQ_SIGNING_PUB", fc, "signing_pub", ""),
		OldPrivateKeyFile: config.EnvOr("DIRQ_SIGNING_KEY_OLD", fc, "signing_key_old", ""),
		OldPublicKeyFile:  config.EnvOr("DIRQ_SIGNING_PUB_OLD", fc, "signing_pub_old", ""),
	}
}

func EnsureServerSigner(cfg Config, log *slog.Logger) (*Signer, error) {
	if cfg.PrivateKeyFile != "" && cfg.PublicKeyFile != "" {
		return LoadSigner(cfg)
	}

	if err := os.MkdirAll(autoGenDir(), 0700); err != nil {
		return nil, fmt.Errorf("create auto-sign dir: %w", err)
	}
	cfg.PrivateKeyFile = filepath.Join(autoGenDir(), "server_signing.key")
	cfg.PublicKeyFile = filepath.Join(autoGenDir(), "server_signing.pub")

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

// SessionTokenTTL is how long a session token remains valid.
const SessionTokenTTL = 24 * time.Hour

// SignToken creates a session token for an agent ID. The token includes a
// timestamp so each issuance produces a unique, expiring token. Format:
// base64(sign(agentID + ":" + unixTimestamp)) + ":" + unixTimestamp
func (s *Signer) SignToken(agentID string) string {
	return s.signTokenAt(agentID, time.Now())
}

func (s *Signer) signTokenAt(agentID string, t time.Time) string {
	ts := fmt.Sprintf("%d", t.Unix())
	payload := agentID + ":" + ts
	sig := ed25519.Sign(s.privateKey, []byte(payload))
	return base64.StdEncoding.EncodeToString(sig) + ":" + ts
}

// VerifyToken checks that a session token was signed by this signer for the
// given agent ID and has not expired. Returns true if valid.
func (s *Signer) VerifyToken(agentID, token string) bool {
	return verifyTokenWith(s.privateKey.Public().(ed25519.PublicKey), agentID, token)
}

// VerifyToken checks that a session token was signed by the server for the
// given agent ID and has not expired. Returns true if valid.
func (v *Verifier) VerifyToken(agentID, token string) bool {
	return verifyTokenWith(v.publicKey, agentID, token)
}

// MultiVerifier tries multiple verifiers in order. A signature is valid
// if any verifier accepts it. Used during key rotation when both old
// and new signing keys should be trusted.
type MultiVerifier struct {
	verifiers []*Verifier
}

// NewMultiVerifier creates a verifier that accepts signatures from any
// of the provided verifiers.
func NewMultiVerifier(verifiers ...*Verifier) *MultiVerifier {
	return &MultiVerifier{verifiers: verifiers}
}

// VerifyServerMessage returns nil if any verifier accepts the message signature.
func (mv *MultiVerifier) VerifyServerMessage(msg *pb.ServerMessage, now time.Time) error {
	var lastErr error
	for _, v := range mv.verifiers {
		if err := v.VerifyServerMessage(msg, now); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no verifiers configured")
}

// VerifyToken returns true if any verifier accepts the token.
func (mv *MultiVerifier) VerifyToken(agentID, token string) bool {
	for _, v := range mv.verifiers {
		if v.VerifyToken(agentID, token) {
			return true
		}
	}
	return false
}

// Verifiers returns the individual verifiers in this MultiVerifier.
// Used when constructing a new MultiVerifier that should include all
// existing verifiers (e.g., during a signing key rotation).
func (mv *MultiVerifier) Verifiers() []*Verifier {
	return append([]*Verifier(nil), mv.verifiers...)
}

// verifyTokenWith validates a session token against a public key.
// Token format: base64(signature) + ":" + unixTimestamp
func verifyTokenWith(pubKey ed25519.PublicKey, agentID, token string) bool {
	lastColon := strings.LastIndex(token, ":")
	if lastColon < 0 {
		return false
	}
	sigB64 := token[:lastColon]
	tsStr := token[lastColon+1:]

	// Check expiry and reject future-dated tokens.
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	if ts > now+30 {
		return false // token timestamp is in the future (clock skew tolerance: 30s)
	}
	if now-ts > int64(SessionTokenTTL.Seconds()) {
		return false
	}

	// Verify signature over "agentID:timestamp".
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	payload := agentID + ":" + tsStr
	return ed25519.Verify(pubKey, []byte(payload), sig)
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
