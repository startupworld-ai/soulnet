package cdp

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// decodeJWS splits a compact JWS into header/payload/signature (base64url).
func decodeJWS(t *testing.T, jwt string) (hdr, payload map[string]any, sig []byte) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %s", jwt)
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("b64: %v", err)
		}
		return b
	}
	if err := json.Unmarshal(dec(parts[0]), &hdr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(dec(parts[1]), &payload); err != nil {
		t.Fatal(err)
	}
	sig = dec(parts[2])
	return
}

func TestPlatformJWTEd25519(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The SDK expects base64(seed || pub) as the API key secret.
	secret := base64.StdEncoding.EncodeToString(append(priv.Seed(), priv.Public().(ed25519.PublicKey)...))
	a, err := newAuth(Credentials{APIKeyID: "key-123", APIKeySecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := a.platformJWT("POST", "api.cdp.coinbase.com", "/platform/v2/evm/accounts")
	if err != nil {
		t.Fatal(err)
	}
	hdr, claims, sig := decodeJWS(t, jwt)
	if hdr["alg"] != "EdDSA" {
		t.Fatalf("alg = %v, want EdDSA", hdr["alg"])
	}
	if hdr["kid"] != "key-123" {
		t.Fatalf("kid = %v", hdr["kid"])
	}
	if claims["sub"] != "key-123" || claims["iss"] != "cdp" {
		t.Fatalf("claims = %v", claims)
	}
	uris := claims["uris"].([]any)
	if uris[0] != "POST api.cdp.coinbase.com/platform/v2/evm/accounts" {
		t.Fatalf("uris = %v", uris)
	}
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	if exp-iat != 120 {
		t.Fatalf("exp-iat = %d, want 120", exp-iat)
	}
	// verify EdDSA signature
	msg := []byte(jwt[:strings.LastIndex(jwt, ".")])
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), msg, sig) {
		t.Fatal("EdDSA signature does not verify")
	}
}

func TestWalletJWTP256(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	walletSecret := base64.StdEncoding.EncodeToString(der)
	a, err := newAuth(Credentials{
		APIKeyID: "key-123", APIKeySecret: base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(make([]byte, 32))),
		WalletSecret: walletSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"network":"base-sepolia","transaction":"0x02f866..."}`)
	jwt, err := a.walletJWT("POST", "api.cdp.coinbase.com", "/platform/v2/evm/accounts/0xabc/send/transaction", body)
	if err != nil {
		t.Fatal(err)
	}
	hdr, claims, sig := decodeJWS(t, jwt)
	if hdr["alg"] != "ES256" {
		t.Fatalf("alg = %v, want ES256", hdr["alg"])
	}
	uris := claims["uris"].([]any)
	if uris[0] != "POST api.cdp.coinbase.com/platform/v2/evm/accounts/0xabc/send/transaction" {
		t.Fatalf("uris = %v", uris)
	}
	rh, _ := claims["reqHash"].(string)
	if len(rh) != 64 {
		t.Fatalf("reqHash = %q, want 64 hex chars", rh)
	}
	// The SDK computes hex sha256(JSON.stringify(sortKeys(body))).
	wantHash := sha256.Sum256([]byte(`{"network":"base-sepolia","transaction":"0x02f866..."}`))
	if rh != strings.ToLower(hexOf(wantHash[:])) {
		t.Fatalf("reqHash mismatch: got %s want %s", rh, hexOf(wantHash[:]))
	}
	// verify ES256 (raw R||S) signature
	msg := []byte(jwt[:strings.LastIndex(jwt, ".")])
	digest := sha256.Sum256(msg)
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&priv.PublicKey, digest[:], r, s) {
		t.Fatal("ES256 signature does not verify")
	}
}

// wallet JWT without a body must omit reqHash (matches "requestData {}" case).
func TestWalletJWTNoBody(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	a, err := newAuth(Credentials{
		APIKeyID: "k", APIKeySecret: base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(make([]byte, 32))),
		WalletSecret: base64.StdEncoding.EncodeToString(der),
	})
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := a.walletJWT("GET", "api.cdp.coinbase.com", "/platform/v2/evm/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, claims, _ := decodeJWS(t, jwt)
	if _, ok := claims["reqHash"]; ok {
		t.Fatal("reqHash must be omitted when there is no body")
	}
}

func hexOf(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexd[v>>4]
		out[i*2+1] = hexd[v&0xf]
	}
	return string(out)
}
