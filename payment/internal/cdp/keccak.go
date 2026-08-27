// Keccak-256 (the pre-NIST variant used by the EVM) and the standard EVM
// address derivation, implemented with the stdlib only — the gateway
// intentionally carries no external Go dependencies, and Go's crypto/sha3
// computes SHA3-256 (FIPS 202), which differs from Keccak-256.
//
// This is the Keccak-f[1600] permutation with the legacy 0x01 pad byte
// (not the 0x06 SHA3 domain separation), 1088-bit rate, 256-bit output.
package cdp

import (
	"encoding/hex"
	"math/big"
)

// Keccak256 returns the Keccak-256 hash of b (32 bytes).
func Keccak256(b []byte) []byte {
	const rate = 136 // 1088-bit rate for a 256-bit output
	var st [25]uint64
	block := make([]byte, rate)
	for len(b) >= rate {
		copy(block, b[:rate])
		absorb(&st, block)
		b = b[rate:]
	}
	// Last block with the multi-rate padding pad10*1: set bit 1 (0x01) after
	// the message and bit 1023 (0x80) as the final bit of the rate portion.
	copy(block, b)
	block[len(b)] ^= 0x01
	block[rate-1] ^= 0x80
	absorb(&st, block)
	out := make([]byte, 32)
	for i := 0; i < 4; i++ {
		putLeUint64(out[i*8:], st[i])
	}
	return out
}

// EVMAddressFromPublicKey derives the 0x EVM address of an elliptic-curve
// public key with the standard scheme: keccak256(0x04 || x || y), last 20
// bytes. Used to bind a wallet-secret signed receipt to the on-chain payer
// address — Coinbase CDP derives its server-signer EVM account addresses the
// same way (eth-account-compatibility, standard EVM derivation).
func EVMAddressFromPublicKey(x, y *big.Int) string {
	pub := make([]byte, 65)
	pub[0] = 4
	x.FillBytes(pub[1:33])
	y.FillBytes(pub[33:65])
	h := Keccak256(pub)
	return "0x" + hex.EncodeToString(h[12:])
}

func absorb(st *[25]uint64, block []byte) {
	for i := 0; i < 17; i++ {
		st[i] ^= leUint64(block[i*8:])
	}
	keccakF1600(st)
}

func leUint64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func putLeUint64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

func rotl64(x uint64, n int) uint64 { return x<<uint(n) | x>>uint(64-n) }

// keccakRC are the 24 iota round constants of Keccak-f[1600].
var keccakRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// keccakRots are the rho rotation offsets for the pi step (24 lanes after A[1]).
var keccakRots = [24]int{
	1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14,
	27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44,
}

// keccakPiln is the rho+pi permutation (the destination lane for each step).
var keccakPiln = [24]int{
	10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4,
	15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1,
}

// keccakF1600 runs the 24-round Keccak-f[1600] permutation in place.
func keccakF1600(a *[25]uint64) {
	var c, d [5]uint64
	for round := 0; round < 24; round++ {
		// θ
		for i := 0; i < 5; i++ {
			c[i] = a[i] ^ a[i+5] ^ a[i+10] ^ a[i+15] ^ a[i+20]
		}
		for i := 0; i < 5; i++ {
			d[i] = c[(i+4)%5] ^ rotl64(c[(i+1)%5], 1)
		}
		for i := 0; i < 25; i++ {
			a[i] ^= d[i%5]
		}
		// ρ + π
		x := a[1]
		for i := 0; i < 24; i++ {
			j := keccakPiln[i]
			tmp := a[j]
			a[j] = rotl64(x, keccakRots[i])
			x = tmp
		}
		// χ
		for j := 0; j < 25; j += 5 {
			for i := 0; i < 5; i++ {
				c[i] = a[j+i]
			}
			for i := 0; i < 5; i++ {
				a[j+i] = c[i] ^ (^c[(i+1)%5] & c[(i+2)%5])
			}
		}
		// ι
		a[0] ^= keccakRC[round]
	}
}
