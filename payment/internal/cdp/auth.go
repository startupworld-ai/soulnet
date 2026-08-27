// Package cdp implements the Coinbase Developer Platform v2 REST client used by
// the payment gateway. Auth follows the official CDP SDK (coinbase/cdp-sdk):
//
//   - Platform JWT (Authorization: Bearer): EdDSA (base64 64-byte Ed25519 seed||pub)
//     or ES256 (PEM EC P-256); header {alg, kid, typ, nonce}; claims
//     {sub, iss:"cdp", uris:["METHOD host/path"], nbf, iat, exp:+120s}.
//   - Wallet JWT (X-Wallet-Auth): ES256 signed with the Wallet Secret (base64 DER
//     EC P-256 PKCS8); header {alg:"ES256", typ:"JWT"}; claims
//     {uris:[...], reqHash? (hex sha256 of JSON.stringify(sortKeys(body))), nbf, iat, jti}.
package cdp

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Credentials holds the three CDP secrets. The wallet secret is required only for
// endpoints that sign or send on behalf of a wallet account.
type Credentials struct {
	APIKeyID     string
	APIKeySecret string // base64 Ed25519 (64B) or PEM EC P-256 private key
	WalletSecret string // base64 DER EC P-256 PKCS8 private key
}

// auth is a pre-parsed signer.
type auth struct {
	cred     Credentials
	edKey    ed25519.PrivateKey
	ecKey    *ecdsa.PrivateKey // platform ES256 (PEM) or wallet (DER base64)
	keyIsEd  bool
	keyIsEC  bool
	walletEC *ecdsa.PrivateKey
}

func newAuth(cred Credentials) (*auth, error) {
	a := &auth{cred: cred}
	// Wallet secret: base64 DER EC P-256 PKCS8.
	if cred.WalletSecret != "" {
		der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred.WalletSecret))
		if err != nil {
			return nil, fmt.Errorf("wallet secret: not base64: %w", err)
		}
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("wallet secret: not a DER PKCS8 private key: %w", err)
		}
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok || ec.Curve.Params().Name != "P-256" {
			return nil, fmt.Errorf("wallet secret: expected EC P-256 private key")
		}
		a.walletEC = ec
	}
	// Platform key: base64 Ed25519 (64 bytes) or PEM EC.
	if b64, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred.APIKeySecret)); err == nil && len(b64) == 64 {
		a.edKey = ed25519.NewKeyFromSeed(b64[:32])
		a.keyIsEd = true
		return a, nil
	}
	blk, _ := pem.Decode([]byte(cred.APIKeySecret))
	if blk != nil {
		key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err == nil {
			if ec, ok := key.(*ecdsa.PrivateKey); ok {
				a.ecKey = ec
				a.keyIsEC = true
				return a, nil
			}
		}
	}
	return nil, fmt.Errorf("api key secret: must be base64 Ed25519 (64B) or PEM EC P-256 private key")
}

// platformJWT builds the Authorization bearer JWT for one request.
func (a *auth) platformJWT(method, host, path string) (string, error) {
	now := time.Now().Unix()
	hdr := map[string]any{
		"typ":   "JWT",
		"nonce": nonce(),
	}
	claims := map[string]any{
		"sub":  a.cred.APIKeyID,
		"iss":  "cdp",
		"uris": []string{method + " " + host + path},
		"nbf":  now,
		"iat":  now,
		"exp":  now + 120,
	}
	switch {
	case a.keyIsEd:
		hdr["alg"] = "EdDSA"
		hdr["kid"] = a.cred.APIKeyID
		return signJWT(hdr, claims, func(msg []byte) ([]byte, error) {
			return ed25519.Sign(a.edKey, msg), nil
		})
	case a.keyIsEC:
		hdr["alg"] = "ES256"
		hdr["kid"] = a.cred.APIKeyID
		return signJWT(hdr, claims, es256Signer(a.ecKey))
	default:
		return "", fmt.Errorf("no platform signing key")
	}
}

// walletJWT builds the X-Wallet-Auth JWT for one request. body is the raw JSON
// request body (may be nil). reqHash is included only when body has content.
func (a *auth) walletJWT(method, host, path string, body []byte) (string, error) {
	if a.walletEC == nil {
		return "", fmt.Errorf("wallet secret not configured")
	}
	now := time.Now().Unix()
	hdr := map[string]any{"alg": "ES256", "typ": "JWT"}
	claims := map[string]any{
		"uris": []string{method + " " + host + path},
		"nbf":  now,
		"iat":  now,
		"jti":  nonce(),
	}
	if len(body) > 0 && string(body) != "null" {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err == nil && len(obj) > 0 {
			sorted, _ := json.Marshal(sortKeys(obj))
			sum := sha256.Sum256(sorted)
			claims["reqHash"] = hex.EncodeToString(sum[:])
		}
	}
	return signJWT(hdr, claims, es256Signer(a.walletEC))
}

// WalletPublicKey returns the uncompressed P-256 public key (0x04||x||y) of the
// wallet secret, or nil when no wallet secret is configured. Receivers use it
// to verify wallet-secret-signed receipts and to derive the wallet's EVM address.
func (a *auth) WalletPublicKey() []byte {
	if a.walletEC == nil {
		return nil
	}
	pub := a.walletEC.PublicKey
	out := make([]byte, 65)
	out[0] = 4
	pub.X.FillBytes(out[1:33])
	pub.Y.FillBytes(out[33:65])
	return out
}

// SignWalletSecret signs msg with the wallet secret (raw ES256 R||S, 64 bytes).
// This is the proof that the caller holds the wallet whose address CDP manages.
func (a *auth) SignWalletSecret(msg []byte) ([]byte, error) {
	if a.walletEC == nil {
		return nil, fmt.Errorf("no wallet secret configured")
	}
	return es256Signer(a.walletEC)(msg)
}

// es256Signer returns a raw-R||S ES256 (ECDSA P-256 / SHA-256) signer.
func es256Signer(key *ecdsa.PrivateKey) func([]byte) ([]byte, error) {
	return func(msg []byte) ([]byte, error) {
		digest := sha256.Sum256(msg)
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			return nil, err
		}
		size := 32
		out := make([]byte, size*2)
		r.FillBytes(out[:size])
		s.FillBytes(out[size:])
		return out, nil
	}
}

// signJWT assembles a compact JWS: base64url(header).base64url(claims).base64url(sig).
func signJWT(hdr, claims map[string]any, sign func(msg []byte) ([]byte, error)) (string, error) {
	hb, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	msg := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	sig, err := sign([]byte(msg))
	if err != nil {
		return "", err
	}
	return msg + "." + enc.EncodeToString(sig), nil
}

// sortKeys recursively sorts object keys so the reqHash matches the SDK's
// JSON.stringify(sortKeys(body)) byte-for-byte.
func sortKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(t))
		for _, k := range keys {
			out[k] = sortKeys(t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = sortKeys(e)
		}
		return out
	default:
		return v
	}
}

// nonce returns a random hex string for the JWT nonce/jti claims.
func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
