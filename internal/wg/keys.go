// Package wg generates and formats WireGuard keys.
package wg

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

type PrivateKey [32]byte
type PublicKey [32]byte

// GenerateKey makes a Curve25519 private key with the clamping WireGuard expects.
func GenerateKey() (PrivateKey, error) {
	var k PrivateKey
	if _, err := rand.Read(k[:]); err != nil {
		return k, err
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	return k, nil
}

func (k PrivateKey) Public() PublicKey {
	var pub PublicKey
	p, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		// X25519 only fails for a low-order point, which a clamped private key
		// multiplied by the base point cannot produce.
		panic("wg: deriving public key: " + err.Error())
	}
	copy(pub[:], p)
	return pub
}

func (k PrivateKey) String() string { return base64.StdEncoding.EncodeToString(k[:]) }
func (k PublicKey) String() string  { return base64.StdEncoding.EncodeToString(k[:]) }

func ParsePrivateKey(s string) (PrivateKey, error) {
	var k PrivateKey
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("wg: key is not base64: %w", err)
	}
	if len(raw) != 32 {
		return k, fmt.Errorf("wg: key is %d bytes, want 32", len(raw))
	}
	copy(k[:], raw)
	return k, nil
}
