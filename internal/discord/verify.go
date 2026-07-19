// Package discord implements a stateless HTTP interactions bot for
// Discord slash commands. No WebSocket gateway, no external Discord
// library — Discord POSTs interaction webhooks to this service and we
// respond synchronously. Stdlib + controller-runtime only.
package discord

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
)

// Verify validates an incoming Discord interaction request against the
// app's Ed25519 public key. Discord signs each request with the
// X-Signature-Ed25519 (hex) and X-Signature-Timestamp headers over the
// concatenation timestamp || body.
//
// publicKeyHex is the hex-encoded application public key (64 hex chars
// → 32 bytes). Returns true only when the signature is valid.
//
// Reference: https://discord.com/developers/docs/interactions/receiving-and-responding#security-and-authorization
func Verify(publicKeyHex, sigHex, ts string, body []byte) (bool, error) {
	if len(publicKeyHex) != ed25519.PublicKeySize*2 {
		return false, fmt.Errorf("bad public key length %d (want %d hex chars)", len(publicKeyHex), ed25519.PublicKeySize*2)
	}
	pub, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return false, fmt.Errorf("decode public key: %w", err)
	}
	if len(sigHex) != ed25519.SignatureSize*2 {
		return false, fmt.Errorf("bad signature length %d (want %d hex chars)", len(sigHex), ed25519.SignatureSize*2)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false, fmt.Errorf("decode signature: %w", err)
	}
	if ts == "" {
		return false, errors.New("empty timestamp header")
	}

	// Verify over timestamp bytes || body. ts is an ASCII decimal string;
	// Discord uses the raw header bytes (no null terminator).
	msg := append([]byte(ts), body...)
	if !ed25519.Verify(pub, msg, sig) {
		return false, nil
	}
	return true, nil
}
