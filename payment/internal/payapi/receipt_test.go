package payapi

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/startupworld-ai/soulnet/payment/internal/cdp"
)

// testCreds builds CDP credentials with a real P-256 wallet secret + an Ed25519
// platform key, so the receipt code paths sign/verify with real keys.
func testCreds(t *testing.T) cdp.Credentials {
	t.Helper()
	walletKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(walletKey)
	if err != nil {
		t.Fatal(err)
	}
	_, platformKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The platform secret is the base64 of the 64-byte Ed25519 key (seed||pub).
	return cdp.Credentials{
		APIKeyID:     "test-key",
		APIKeySecret: base64.StdEncoding.EncodeToString(platformKey),
		WalletSecret: base64.StdEncoding.EncodeToString(der),
	}
}

// mintReceipt reproduces exactly what POST /v2/pay/join.receipt does (same
// signer path), so the round trip tests the handler's crypto without the HTTP
// layer.
func mintReceipt(t *testing.T, client *cdp.Client, fp, txHash, payer string) *JoinPaymentProofReq {
	t.Helper()
	msg, err := json.Marshal(map[string]string{"fp": fp, "tx_hash": txHash, "payer": payer})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := client.SignWalletMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	return &JoinPaymentProofReq{
		Message: string(msg),
		Pubkey:  hex.EncodeToString(client.WalletPublicKey()),
		Sig:     hex.EncodeToString(sig),
	}
}

func TestJoinReceiptRoundTrip(t *testing.T) {
	client, err := cdp.NewClient(testCreds(t))
	if err != nil {
		t.Fatal(err)
	}
	fp := "aGVsbG9fd29ybGQ"
	txHash := "0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7"
	// The payer must be the EVM address derived from the wallet's public key.
	pub, err := parseUncompressedP256(hex.EncodeToString(client.WalletPublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	payer := cdp.EVMAddressFromPublicKey(pub.X, pub.Y)

	receipt := mintReceipt(t, client, fp, txHash, payer)

	got, err := verifyJoinReceipt(receipt)
	if err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	if !stringsEqualFold(got, payer) {
		t.Fatalf("payer = %s, want %s", got, payer)
	}

	// Tampering: flip one signature byte → must fail.
	bad := *receipt
	raw, _ := hex.DecodeString(bad.Sig)
	raw[0] ^= 0x01
	bad.Sig = hex.EncodeToString(raw)
	if _, err := verifyJoinReceipt(&bad); err == nil {
		t.Fatal("tampered signature accepted")
	}

	// A receipt claiming a DIFFERENT payer must fail (pubkey does not derive to it).
	other := *receipt
	otherMsg := map[string]string{"fp": fp, "tx_hash": txHash, "payer": "0x0000000000000000000000000000000000000001"}
	rawMsg, _ := json.Marshal(otherMsg)
	sig, _ := client.SignWalletMessage(rawMsg)
	other.Message = string(rawMsg)
	other.Sig = hex.EncodeToString(sig)
	if _, err := verifyJoinReceipt(&other); err == nil {
		t.Fatal("receipt whose pubkey does not derive to the payer accepted")
	}

	// Garbage inputs must fail cleanly.
	if _, err := verifyJoinReceipt(&JoinPaymentProofReq{Message: "{}", Pubkey: "zz", Sig: "zz"}); err == nil {
		t.Fatal("garbage receipt accepted")
	}
	if _, err := verifyJoinReceipt(nil); err == nil {
		t.Fatal("nil receipt accepted")
	}
}

func stringsEqualFold(a, b string) bool {
	return len(a) == len(b) && (a == b || lowerHex(a) == lowerHex(b))
}

func lowerHex(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
