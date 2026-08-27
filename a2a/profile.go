package a2a

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Skill is one structured skill entry on the capability card.
type Skill struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Tags   []string `json:"tags"`
	Desc   string   `json:"desc"`
	Type   string   `json:"type,omitempty"`   // private (proprietary, the selling point) | generic (generic, every AI can do it)
	Hidden bool     `json:"hidden,omitempty"` // hidden from the outside: filtered out when publishing; still usable inside the alter
}

// Context is the owner's exclusive, hard-to-migrate private context (the bulk of a capability card's value).
type Context struct {
	Title  string `json:"title"`
	Desc   string `json:"desc,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
}

// Offering is a service the owner can provide externally (simple name + description); it answers "what can be bought" and is more concrete than a skill.
type Offering struct {
	Name   string `json:"name"`
	Desc   string `json:"desc,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
}

// Profile is the alter's capability declaration (module A): decoupled from the slim Card, self-signed, evolves independently.
// Card.DescURL points to it. The structured skills feed the hub's inverted-index pre-filter; intro feeds local fine filtering / the alter.
type Profile struct {
	V            int        `json:"v"`
	Fingerprint  string     `json:"fingerprint"`
	Tags         []string   `json:"tags,omitempty"`          // owner's basic attribute tags
	Summary      string     `json:"summary,omitempty"`       // one sentence on "what I can take on"
	DistillScore int        `json:"distill_score,omitempty"` // distillation score 0-100: how well the alter knows its owner (trust signal)
	Skills       []Skill    `json:"skills"`
	Contexts     []Context  `json:"contexts,omitempty"`     // private contexts
	Services     []Offering `json:"services,omitempty"`     // services offered (buyable externally)
	FriendCount  int        `json:"friend_count,omitempty"` // friend count (network density)
	FriendTags   []string   `json:"friend_tags,omitempty"`  // aggregated friend tags, e.g. "BP writing·3"
	// Homepage is the owner's public homepage URL (<tunnel base>/u): injected by the daemon on publish (empty when
	// the tunnel is off); clicking this person in the discovery plaza jumps straight to their homepage. omitempty —
	// old cards without it keep the same signing bytes, so cards published by old clients still verify on a new relay.
	Homepage  string    `json:"homepage,omitempty"`
	Intro     string    `json:"intro"`
	Accepting bool      `json:"accepting"`
	UpdatedAt time.Time `json:"updated_at"`
	Sig       string    `json:"sig,omitempty"`
	// USDCAddress is the alter's public USDC (Base) wallet address, published so
	// other agents can pay it without asking. omitempty keeps old profiles'
	// signing bytes unchanged (the field only enters the signature once set).
	USDCAddress string `json:"usdc_address,omitempty"`
}

// PublicCopy returns the copy used for publishing: every skill / context marked Hidden is filtered out (hidden from the outside).
func (p *Profile) PublicCopy() *Profile {
	c := *p
	c.Skills = nil
	for _, s := range p.Skills {
		if !s.Hidden {
			c.Skills = append(c.Skills, s)
		}
	}
	c.Contexts = nil
	for _, x := range p.Contexts {
		if !x.Hidden {
			c.Contexts = append(c.Contexts, x)
		}
	}
	c.Services = nil
	for _, sv := range p.Services {
		if !sv.Hidden {
			c.Services = append(c.Services, sv)
		}
	}
	return &c
}

// signingBytes returns the deterministic serialization without Sig (fixed struct field order, slices keep their order).
func (p *Profile) signingBytes() []byte {
	c := *p
	c.Sig = ""
	b, _ := json.Marshal(&c)
	return b
}

// Sign self-signs with the Ed25519 private key.
func (p *Profile) Sign(priv ed25519.PrivateKey) {
	p.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, p.signingBytes()))
}

// Verify checks the signature with the card owner's Ed25519 public key (base64) and that the fingerprint is consistent.
func (p *Profile) Verify(edPubB64 string) error {
	pub, err := DecodeKey(edPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("能力名片公钥非法")
	}
	if Fingerprint(ed25519.PublicKey(pub)) != p.Fingerprint {
		return fmt.Errorf("能力名片指纹与公钥不符")
	}
	sig, err := DecodeKey(p.Sig)
	if err != nil {
		return fmt.Errorf("能力名片签名非 base64")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), p.signingBytes(), sig) {
		return fmt.Errorf("能力名片签名校验失败")
	}
	return nil
}

// ProfileStore manages a2a/profile.json (mine) and a2a/profiles/<fp>.json (friends).
type ProfileStore struct {
	baseDir string
	mu      sync.Mutex
}

func NewProfileStore(baseDir string) *ProfileStore { return &ProfileStore{baseDir: baseDir} }

func (s *ProfileStore) minePath() string { return filepath.Join(s.baseDir, "a2a", "profile.json") }

// publishedFlagPath is the local marker file for "is the local card published to the plaza" (exists = published).
func (s *ProfileStore) publishedFlagPath() string {
	return filepath.Join(s.baseDir, "a2a", "profile.published")
}

// IsPublished reports whether the local card is currently published (judged by the local marker file).
func (s *ProfileStore) IsPublished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := os.Stat(s.publishedFlagPath())
	return err == nil
}

// SetPublished writes/removes the local published marker.
func (s *ProfileStore) SetPublished(published bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.publishedFlagPath()
	if !published {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("1"), 0o644)
}

func (s *ProfileStore) friendPath(fp string) (string, error) {
	if fp == "" || strings.ContainsAny(fp, `/\`) || strings.Contains(fp, "..") {
		return "", fmt.Errorf("非法指纹: %q", fp)
	}
	return filepath.Join(s.baseDir, "a2a", "profiles", fp+".json"), nil
}

func readProfile(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("解析能力名片 %s: %w", path, err)
	}
	return &p, nil
}

func writeProfile(path string, p *Profile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func (s *ProfileStore) Mine() (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readProfile(s.minePath())
}

func (s *ProfileStore) SaveMine(p *Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeProfile(s.minePath(), p)
}

func (s *ProfileStore) Friend(fp string) (*Profile, error) {
	p, err := s.friendPath(fp)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return readProfile(p)
}

func (s *ProfileStore) SaveFriend(fp string, prof *Profile) error {
	p, err := s.friendPath(fp)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeProfile(p, prof)
}
