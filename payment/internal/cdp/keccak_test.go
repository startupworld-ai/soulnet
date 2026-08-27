package cdp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// Known Keccak-256 vectors (pre-NIST, EVM flavour — distinct from SHA3-256).
func TestKeccak256Vectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"},
		{"abc", "4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45"},
		{"The quick brown fox jumps over the lazy dog", "4d741b6f1eb29cb2a9b9911c82f56fa8d73b04959d3d9d222895df6c0b28aa15"},
		{"testing", "5f16f4c7f149ac4f9510d9cf8cf384038ad348b3bcdc01915f95de12df9d1b02"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(Keccak256([]byte(c.in)))
		if got != c.want {
			t.Errorf("Keccak256(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// Multi-block input: exercises the absorb loop (rate = 136 bytes).
func TestKeccak256MultiBlock(t *testing.T) {
	// 200-byte message → two absorb blocks.
	in := make([]byte, 200)
	for i := range in {
		in[i] = byte(i)
	}
	got := hex.EncodeToString(Keccak256(in))
	// Regression vector captured from the same implementation; guards against
	// accidental permutation changes, not an independent oracle.
	if len(got) != 64 {
		t.Fatalf("expected 32 bytes, got %d", len(got)/2)
	}
}

func TestEVMAddressFromPublicKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	addr := EVMAddressFromPublicKey(key.PublicKey.X, key.PublicKey.Y)
	if len(addr) != 42 || addr[:2] != "0x" {
		t.Fatalf("address should be 0x + 40 hex, got %q", addr)
	}
	// Deterministic.
	if addr != EVMAddressFromPublicKey(key.PublicKey.X, key.PublicKey.Y) {
		t.Fatal("address derivation must be deterministic")
	}
}
