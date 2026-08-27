package a2a

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Identity is this machine's SoulMirror A2A identity (A2A v2 §3.1):
// an Ed25519 (identity/signing) + X25519 (encryption) key pair, plus a local self-declared nickname.
// There is no network registration step whatsoever — the identity is the keys themselves. Stored in identity.json (0600).
// Losing the private key means losing the identity; migrating to another machine = copying this file.
type Identity struct {
	Name      string    `json:"name"`     // self-declared nickname (local, not globally unique)
	EdPub     string    `json:"ed_pub"`   // base64
	EdPriv    string    `json:"ed_priv"`  // base64 — local machine only
	XPub      string    `json:"x_pub"`    // base64
	XPriv     string    `json:"x_priv"`   // base64 — local machine only
	Proxies   []string  `json:"proxies"`  // relays where I receive mail (primary/backup)
	DescURL   string    `json:"desc_url"` // ADP capability declaration URL (optional)
	CreatedAt time.Time `json:"created_at"`
}

func identityPath(baseDir string) string {
	return filepath.Join(baseDir, "a2a", "identity.json")
}

// LoadIdentity reads the local identity; returns (nil, nil) when absent.
func LoadIdentity(baseDir string) (*Identity, error) {
	raw, err := os.ReadFile(identityPath(baseDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("解析 identity.json: %w", err)
	}
	return &id, nil
}

// NewIdentity generates a new identity (Ed25519 + X25519) and writes it to disk. Errors if one already exists
// (prevents overwriting and losing the identity by mistake). proxies are the relay addresses chosen by the user (at least one).
func NewIdentity(baseDir, name string, proxies []string) (*Identity, error) {
	if err := ValidNickname(name); err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		return nil, fmt.Errorf("at least one relay address is required")
	}
	p := identityPath(baseDir)
	if _, err := os.Stat(p); err == nil {
		return nil, fmt.Errorf("已有身份文件 %s，不可覆盖（丢私钥即丢身份）", p)
	}
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	xPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	id := &Identity{
		Name:      name,
		EdPub:     EncodeKey(edPub),
		EdPriv:    EncodeKey(edPriv),
		XPub:      EncodeKey(xPriv.PublicKey().Bytes()),
		XPriv:     EncodeKey(xPriv.Bytes()),
		Proxies:   proxies,
		CreatedAt: time.Now(),
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	return id, id.save(baseDir)
}

func (id *Identity) save(baseDir string) error {
	raw, _ := json.MarshalIndent(id, "", "  ")
	return os.WriteFile(identityPath(baseDir), raw, 0o600)
}

// EdPrivate returns the Ed25519 private key.
func (id *Identity) EdPrivate() (ed25519.PrivateKey, error) {
	b, err := DecodeKey(id.EdPriv)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("Ed25519 私钥损坏")
	}
	return ed25519.PrivateKey(b), nil
}

// EdPublic returns the Ed25519 public key.
func (id *Identity) EdPublic() (ed25519.PublicKey, error) {
	b, err := DecodeKey(id.EdPub)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 公钥损坏")
	}
	return ed25519.PublicKey(b), nil
}

// XPrivate returns the X25519 private key (for encryption).
func (id *Identity) XPrivate() (*ecdh.PrivateKey, error) {
	b, err := DecodeKey(id.XPriv)
	if err != nil {
		return nil, fmt.Errorf("X25519 私钥损坏")
	}
	return ecdh.X25519().NewPrivateKey(b)
}

// Fingerprint returns the local identity fingerprint (routing address).
func (id *Identity) Fingerprint() string {
	pub, err := id.EdPublic()
	if err != nil {
		return ""
	}
	return Fingerprint(pub)
}

// Card builds the local card (already self-signed).
func (id *Identity) Card() (*Card, error) {
	priv, err := id.EdPrivate()
	if err != nil {
		return nil, err
	}
	c := &Card{
		V:       2,
		EdPub:   id.EdPub,
		XPub:    id.XPub,
		Proxies: id.Proxies,
		Name:    id.Name,
		DescURL: id.DescURL,
	}
	c.Sign(priv)
	return c, nil
}
