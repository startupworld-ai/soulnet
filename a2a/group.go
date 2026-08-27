// Group messaging (A2A wire spec §14): sender-key fan-out groups.
//
// Architecture (the WhatsApp / Matrix-Megolm family, adapted to soulnet):
//
//   - A group is a signed ROSTER: the owner's Ed25519 key signs the member list (full
//     cards, so members need not be mutual friends). The roster lives on the group's
//     home relay ("the member list is shared information"), versioned monotonically.
//   - Every member holds one SENDER KEY per group: a 32-byte HMAC-SHA256 chain that
//     ratchets forward once per message (forward secrecy). It is distributed once to
//     each co-member over the existing pairwise E2E channel (`group_key` messages).
//   - Sending = encrypt ONCE with the current chain step (AES-256-GCM) and upload ONE
//     group envelope; the relay verifies the sender's signature + membership and copies
//     the ciphertext into every member's existing mailbox. The relay never sees content.
//   - Removing a member bumps the sender-key EPOCH: every remaining member generates a
//     fresh chain and redistributes it, so the removed member cannot read new messages.
//
// The group envelope reuses Envelope with GID set and To empty: the sender signs
// gid+ts+cipher (not a recipient), the relay stamps To per copy for routing only.
package a2a

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Group message types (inner envelope `type`), on top of the pairwise set in wire.go.
const (
	TypeGroupInvite = "group_invite" // pairwise, owner → invitee: carries the signed roster (Group field); the invitee joins and distributes its sender key
	TypeGroupKey    = "group_key"    // pairwise, member → member: carries the sender's chain key for one group (GKey field)
	TypeGroupLeave  = "group_leave"  // pairwise, member → owner: "remove me from the roster" (body = optional note)
	TypeGroupUpdate = "group_update" // fan-out, owner → group: "the roster changed, refetch it" (body = note; triggers rekey checks)
)

// MaxGroupMembers caps the roster size (fan-out cost is linear; raise deliberately).
const MaxGroupMembers = 128

// MaxGroupNameLen caps the group display name.
const MaxGroupNameLen = 64

// maxChainSkip caps how far GroupOpen ratchets forward in one step (poison-message guard).
const maxChainSkip = 4096

// maxSkippedKeys caps the cached out-of-order message keys per sender (oldest pruned).
const maxSkippedKeys = 512

var groupIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewGroupID returns a fresh group ID: 16 random bytes as 32 lowercase hex characters.
func NewGroupID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidGroupID reports whether s is a well-formed group ID.
func ValidGroupID(s string) bool { return groupIDRe.MatchString(s) }

// GroupConvKey is the conversation-archive key of a group ("g_" + gid): it shares
// ConvStore with pairwise threads and cannot collide with a fingerprint (fingerprints
// are 22 characters, this is 34).
func GroupConvKey(gid string) string { return "g_" + gid }

// ——— Roster ———

// GroupRoster is the signed member list of one group — the single source of truth for
// "who is in", published on the group's home relay by the owner.
//
// Members are full cards (public keys + relays), so any member can encrypt to and verify
// any other member without them being friends. The owner's own card must be a member.
type GroupRoster struct {
	V        int       `json:"v"`        // roster format version, currently 1
	GroupID  string    `json:"group_id"` // 32 hex chars, see NewGroupID
	Name     string    `json:"name"`     // display name (≤ MaxGroupNameLen)
	OwnerPub string    `json:"owner_pk"` // owner's Ed25519 public key, base64 (signs the roster)
	Relay    string    `json:"relay"`    // group home relay base URL (where the roster lives and group mail is posted)
	Version  int       `json:"version"`  // monotonically increasing; the relay rejects non-increasing republishes
	Members  []*Card   `json:"members"`  // full member cards, owner included
	TS       time.Time `json:"ts"`
	// Profile is the governance layer (§14.7): switches + free-text rules, signed with
	// the roster. nil on pre-profile groups = everything allowed, chat room.
	Profile *GroupProfile `json:"profile,omitempty"`
	Sig     string        `json:"sig"` // owner's signature over signingBytes()
}

// signingBytes is the canonical signing string of a roster. Member cards are folded in
// through their EncodeURI form (deterministic: url.Values.Encode sorts keys), in the
// exact slice order the owner published.
func (g *GroupRoster) signingBytes() []byte {
	parts := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		parts = append(parts, m.EncodeURI())
	}
	base := fmt.Sprintf("a2a-group-v%d\n%s\n%s\n%s\n%s\n%d\n%s\n%s",
		g.V, g.GroupID, g.Name, g.OwnerPub, g.Relay, g.Version,
		g.TS.UTC().Format(time.RFC3339Nano), strings.Join(parts, "\n"))
	// The profile (§14.7) signs with the roster; a nil profile appends nothing, so
	// pre-profile rosters keep their exact signing bytes.
	if g.Profile != nil {
		base += "\n" + g.Profile.canonical()
	}
	return []byte(base)
}

// Sign signs the roster with the owner's Ed25519 private key.
func (g *GroupRoster) Sign(priv ed25519.PrivateKey) {
	g.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, g.signingBytes()))
}

// OwnerFp returns the owner's fingerprint ("" when OwnerPub is malformed).
func (g *GroupRoster) OwnerFp() string {
	pub, err := DecodeKey(g.OwnerPub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ""
	}
	return Fingerprint(pub)
}

// Member returns the card of the member with fingerprint fp, or nil.
func (g *GroupRoster) Member(fp string) *Card {
	for _, m := range g.Members {
		if f, err := m.Fingerprint(); err == nil && f == fp {
			return m
		}
	}
	return nil
}

// MemberFps returns every member fingerprint (sorted, deterministic).
func (g *GroupRoster) MemberFps() []string {
	out := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		if f, err := m.Fingerprint(); err == nil {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// Verify checks the roster end to end: shape, every member card's self-signature, the
// owner being a member, and the owner's roster signature. Both the relay and every
// receiving member run this — nobody trusts a roster that does not verify.
func (g *GroupRoster) Verify() error {
	if g.V != 1 {
		return fmt.Errorf("unsupported roster version: %d", g.V)
	}
	if !ValidGroupID(g.GroupID) {
		return fmt.Errorf("invalid group id")
	}
	name := strings.TrimSpace(g.Name)
	if name == "" || len([]rune(name)) > MaxGroupNameLen {
		return fmt.Errorf("group name must be 1-%d characters", MaxGroupNameLen)
	}
	if g.Version < 1 {
		return fmt.Errorf("roster version must be >= 1")
	}
	if len(g.Members) < 1 || len(g.Members) > MaxGroupMembers {
		return fmt.Errorf("member count must be 1-%d", MaxGroupMembers)
	}
	ownerPub, err := DecodeKey(g.OwnerPub)
	if err != nil || len(ownerPub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid owner public key")
	}
	seen := map[string]bool{}
	for _, m := range g.Members {
		if m == nil {
			return fmt.Errorf("nil member card")
		}
		if err := m.Verify(); err != nil {
			return fmt.Errorf("member card: %w", err)
		}
		fp, err := m.Fingerprint()
		if err != nil {
			return err
		}
		if seen[fp] {
			return fmt.Errorf("duplicate member %s", ShortFp(fp))
		}
		seen[fp] = true
	}
	if !seen[g.OwnerFp()] {
		return fmt.Errorf("the owner must be a member of its own group")
	}
	if g.Profile != nil {
		fps := make([]string, 0, len(seen))
		for fp := range seen {
			fps = append(fps, fp)
		}
		if err := g.Profile.Validate(fps); err != nil {
			return fmt.Errorf("profile: %w", err)
		}
	}
	sig, err := DecodeKey(g.Sig)
	if err != nil {
		return fmt.Errorf("roster signature is not base64")
	}
	if !ed25519.Verify(ownerPub, g.signingBytes(), sig) {
		return fmt.Errorf("roster signature check failed")
	}
	return nil
}

// ——— Sender key: HMAC-SHA256 chain ratchet ———

// GroupSenderKey is MY sending chain for one group: epoch + position + current chain key.
// Advances once per message sent; a fresh epoch (rekey) replaces chain and resets Index.
type GroupSenderKey struct {
	Epoch int    `json:"epoch"` // bumped on every rekey (member removal)
	Index int    `json:"index"` // next message index to send
	Chain string `json:"chain"` // current 32-byte chain key, base64
}

// NewGroupSenderKey generates a fresh chain for the given epoch.
func NewGroupSenderKey(epoch int) (*GroupSenderKey, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &GroupSenderKey{Epoch: epoch, Index: 0, Chain: EncodeKey(b)}, nil
}

// GroupRecvState is what I hold for ONE sender in ONE group: their chain at my current
// read position, plus cached message keys for messages that arrived out of order.
type GroupRecvState struct {
	Epoch   int            `json:"epoch"`
	Index   int            `json:"index"`             // next expected message index
	Chain   string         `json:"chain"`             // chain key at Index, base64
	Skipped map[int]string `json:"skipped,omitempty"` // index → message key (base64) for skipped-over messages
}

func groupMsgKey(chain []byte) []byte {
	m := hmac.New(sha256.New, chain)
	m.Write([]byte{1})
	return m.Sum(nil)
}

func groupNextChain(chain []byte) []byte {
	m := hmac.New(sha256.New, chain)
	m.Write([]byte{2})
	return m.Sum(nil)
}

// groupCipherBlob is the JSON carried (base64) in a group envelope's Cipher field.
type groupCipherBlob struct {
	E int    `json:"e"` // sender-key epoch
	I int    `json:"i"` // message index in the chain
	C string `json:"c"` // base64(nonce || AES-256-GCM ciphertext) under the message key at index I
}

func gcmSeal(key, plain []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)), nil
}

func gcmOpen(key []byte, ctB64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ctB64)
	if err != nil {
		return nil, fmt.Errorf("ciphertext is not base64")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (key mismatch or tampering)")
	}
	return plain, nil
}

// GroupSeal encrypts msg with the message key at sk's current position, then advances the
// chain (ratchet). Returns the value for Envelope.Cipher.
func GroupSeal(sk *GroupSenderKey, msg *Message) (string, error) {
	chain, err := DecodeKey(sk.Chain)
	if err != nil || len(chain) != 32 {
		return "", fmt.Errorf("invalid sender chain key")
	}
	plain, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	ct, err := gcmSeal(groupMsgKey(chain), plain)
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(groupCipherBlob{E: sk.Epoch, I: sk.Index, C: ct})
	if err != nil {
		return "", err
	}
	sk.Chain = EncodeKey(groupNextChain(chain))
	sk.Index++
	return base64.StdEncoding.EncodeToString(blob), nil
}

// ErrGroupKeyMissing marks "cannot decrypt YET": the sender's chain key (or a newer
// epoch of it) has not arrived. Callers treat it as transient and retry after the
// pairwise `group_key` message lands.
var ErrGroupKeyMissing = fmt.Errorf("sender key not available yet")

// GroupOpen decrypts a group cipher with the receive state of its sender, ratcheting st
// forward and caching skipped message keys (out-of-order delivery). st is mutated on
// success and must be persisted by the caller.
func GroupOpen(st *GroupRecvState, cipherB64 string) (*Message, error) {
	raw, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return nil, fmt.Errorf("group cipher is not base64")
	}
	var blob groupCipherBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil, fmt.Errorf("group cipher blob: %w", err)
	}
	if blob.E > st.Epoch {
		return nil, fmt.Errorf("%w: message epoch %d > held epoch %d", ErrGroupKeyMissing, blob.E, st.Epoch)
	}
	if blob.E < st.Epoch {
		return nil, fmt.Errorf("message from a superseded sender-key epoch %d (held %d)", blob.E, st.Epoch)
	}
	var msgKey []byte
	switch {
	case blob.I < st.Index:
		k, ok := st.Skipped[blob.I]
		if !ok {
			return nil, fmt.Errorf("message index %d already consumed (replay or lost skip key)", blob.I)
		}
		msgKey, err = DecodeKey(k)
		if err != nil {
			return nil, fmt.Errorf("corrupt skipped key")
		}
		delete(st.Skipped, blob.I)
	default:
		if blob.I-st.Index > maxChainSkip {
			return nil, fmt.Errorf("message index %d skips too far ahead of %d", blob.I, st.Index)
		}
		chain, err := DecodeKey(st.Chain)
		if err != nil || len(chain) != 32 {
			return nil, fmt.Errorf("invalid receive chain key")
		}
		if st.Skipped == nil {
			st.Skipped = map[int]string{}
		}
		for i := st.Index; i < blob.I; i++ {
			st.Skipped[i] = EncodeKey(groupMsgKey(chain))
			chain = groupNextChain(chain)
		}
		pruneSkipped(st.Skipped)
		msgKey = groupMsgKey(chain)
		st.Chain = EncodeKey(groupNextChain(chain))
		st.Index = blob.I + 1
	}
	plain, err := gcmOpen(msgKey, blob.C)
	if err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// pruneSkipped drops the OLDEST cached keys once the cache exceeds maxSkippedKeys
// (messages that old are considered lost).
func pruneSkipped(sk map[int]string) {
	if len(sk) <= maxSkippedKeys {
		return
	}
	idx := make([]int, 0, len(sk))
	for i := range sk {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx[:len(idx)-maxSkippedKeys] {
		delete(sk, i)
	}
}

// GroupKeyDist is the payload of a pairwise `group_key` message: the sender's chain key
// for the group named by Message.GID, at index 0 of the given epoch.
type GroupKeyDist struct {
	Epoch int `json:"epoch"`
	// Index is the chain position the Chain below is AT (the sender's next message
	// index). A late joiner gets the sender's CURRENT ratcheted chain, not the
	// epoch's origin - storing it as index 0 desynchronizes the ratchet and every
	// later message fails to decrypt. Absent (0) = an unadvanced chain.
	Index int    `json:"index,omitempty"`
	Chain string `json:"chain"` // 32-byte chain key at Index, base64
}

// ——— Group envelope (outer, relay-visible) ———

// groupEnvelopeSigningBytes: the sender signs gid+ts+cipher — NOT a recipient. The relay
// stamps To per fan-out copy for routing only; receivers re-verify this same form.
func groupEnvelopeSigningBytes(gid string, ts time.Time, cipherB64 string) []byte {
	return []byte("a2a-group-env-v1\n" + gid + "\n" + ts.UTC().Format(time.RFC3339Nano) + "\n" + cipherB64)
}

// SealGroupEnvelope wraps an already-GroupSeal'ed cipher into a signed group envelope.
func SealGroupEnvelope(id *Identity, gid, cipherB64 string) (*Envelope, error) {
	priv, err := id.EdPrivate()
	if err != nil {
		return nil, err
	}
	env := &Envelope{V: 2, GID: gid, From: id.EdPub, TS: time.Now()}
	env.Cipher = cipherB64
	env.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, groupEnvelopeSigningBytes(gid, env.TS, cipherB64)))
	return env, nil
}

// VerifyGroupEnvelope checks a group envelope's shape and sender signature and returns
// the sender's fingerprint. Run by the relay before fan-out AND by every receiver
// (never trust the relay).
func (e *Envelope) VerifyGroupEnvelope() (senderFp string, err error) {
	if e.V != 2 {
		return "", fmt.Errorf("unsupported envelope version: %d", e.V)
	}
	if !ValidGroupID(e.GID) {
		return "", fmt.Errorf("invalid group id")
	}
	fromPub, err := DecodeKey(e.From)
	if err != nil || len(fromPub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("invalid sender public key")
	}
	sig, err := DecodeKey(e.Sig)
	if err != nil {
		return "", fmt.Errorf("signature is not base64")
	}
	if !ed25519.Verify(fromPub, groupEnvelopeSigningBytes(e.GID, e.TS, e.Cipher), sig) {
		return "", fmt.Errorf("group envelope signature check failed")
	}
	return Fingerprint(fromPub), nil
}
