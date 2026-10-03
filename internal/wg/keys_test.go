package wg

import "testing"

func TestKeyRoundTrip(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// Clamping is what makes a key valid for WireGuard rather than merely random.
	if k[0]&7 != 0 || k[31]&128 != 0 || k[31]&64 == 0 {
		t.Errorf("private key not clamped: %x", k)
	}
	back, err := ParsePrivateKey(k.String())
	if err != nil || back != k {
		t.Errorf("round trip failed: %v", err)
	}
	if k.Public() != k.Public() {
		t.Error("public key derivation is not deterministic")
	}
	other, _ := GenerateKey()
	if k.Public() == other.Public() {
		t.Error("two keys produced the same public key")
	}
	if len(k.Public().String()) != 44 {
		t.Errorf("public key should be 44 base64 chars, got %d", len(k.Public().String()))
	}
	if _, err := ParsePrivateKey("not-base64!"); err == nil {
		t.Error("want an error for non-base64")
	}
	if _, err := ParsePrivateKey("c2hvcnQ="); err == nil {
		t.Error("want an error for a short key")
	}
}
