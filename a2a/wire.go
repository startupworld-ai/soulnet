// Package a2a implements SoulMirror interconnect (A2A v2 spec): key identity, cards,
// end-to-end encrypted envelopes, local storage for friends / conversations / pending
// requests / missions, and send/receive plus auto-reply (Responder, via AgentRunner — the
// single intelligence outlet) through a self-hostable relay (message broker).
//
// This file is the [wire protocol]: envelopes, encryption, cards and request signing shared
// by the daemon and the relay. Architecture inspired by Agent Network Protocol (ANP: key identity + E2E + message broker) — no ANP code or spec text is reused and the wire format is not ANP-compatible (see THIRD_PARTY_NOTICES.md);
// implemented on the Go standard library, zero new dependencies.
package a2a

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Message types (inner envelope `type`). Minimal design: no distinction between "conversation"
// and "task" — everything is a message, and the alter decides per the diplomatic protocol whether
// to answer, act, or relay. The remaining types exist only for mechanics.
const (
	TypeText          = "text"           // message between alters (conversation / job / relay — the receiver decides)
	TypeFriendRequest = "friend_request" // friend request (body=verification text, carries the requester's card)
	TypeFriendAccept  = "friend_accept"  // request accepted (carries the accepter's card)
	TypeTyping        = "typing"         // processing signal (loading indicator, not archived; body=on/off)
	TypeTask          = "task"           // task card (body=brief, Task=mission contract summary)
	TypeMissionUpdate = "mission_update" // mission status change (Task.MissionID+Status; body=note; may carry a delivery attachment)
	TypeArtifactChunk = "artifact_chunk" // large-file chunk (>700KB is split into chunks, each self-contained with metadata)
	TypeMissionBid    = "mission_bid"    // bid negotiation (Task.MissionID+Budget+Status=bid_proposed/bid_accepted; body=note)
	TypeAppShare      = "app_share"      // app-share notice (Share carries action/app/tunnel_url; mechanical, does not wake the alter)
	TypeGroupVoices   = "group_voices"   // seat roster metadata: the sender's enabled seat-agent names in this group (not archived; Voices field)
)

// App-share actions (Share.Action of TypeAppShare messages).
const (
	AppShareGranted = "granted" // the sharer granted this side an app (carries app name/title/sharer tunnel URL)
	AppShareRevoked = "revoked" // the sharer revoked the grant (this side removes the entry accordingly)
)

// AppShare is the "app × friend sharing" notification payload (inner envelope Share field).
// Sharing = the sharer grants an app to a friend (identified by fingerprint); the friend enters
// from their own SoulMirror and accesses the live app on the sharer's machine with a short-lived
// access ticket signed by their own A2A private key. See
// docs/superpowers/specs/2026-07-24-app-friend-sharing-design.html.
type AppShare struct {
	Action    string `json:"action"`               // granted | revoked
	App       string `json:"app"`                  // app name (apps/<name>)
	AppTitle  string `json:"app_title,omitempty"`  // app title (for display)
	TunnelURL string `json:"tunnel_url,omitempty"` // sharer's remote-access (reverse tunnel) base URL; required for granted
}

// Bid negotiation sub-states (Task.Status of TypeMissionBid messages; independent of the main mission state machine, not in missionOrder).
const (
	MissionBidProposed = "bid_proposed" // propose / counter a budget (either side may send, back and forth)
	MissionBidAccepted = "bid_accepted" // accept the peer's latest proposal → budget settled, settlement unblocked
)

// Message is the [inner envelope] — encrypted so only the two endpoints can read it; the relay cannot.
type Message struct {
	ID     string    `json:"id"`
	From   string    `json:"from"` // sender's public-key fingerprint
	To     string    `json:"to"`   // recipient's public-key fingerprint
	ConvID string    `json:"conv_id,omitempty"`
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`
	Body   string    `json:"body"`
	// Auto is the loop guard: set true on the alter's automatic replies; incoming auto messages are subject to the multi-round cap.
	Auto bool `json:"auto,omitempty"`
	// Card carries the sender's card in friend_request / friend_accept so the peer can create a record.
	Card *Card `json:"card,omitempty"`
	// Payment carries the paid-join proof on a group_join (join policy "paid"):
	// the on-chain USDC transfer hash + amount + recipient the applicant paid.
	// Only present on group_join; verified by the group owner's node before approval.
	Payment *JoinPayment `json:"payment,omitempty"`
	// Attachment: any message may carry one file (tables / reports the alter produced after finishing a job).
	// Artifact is the file content in base64 (≤ relay cap; cleared once written to disk so the jsonl does not bloat).
	Artifact     string `json:"artifact,omitempty"`
	ArtifactName string `json:"artifact_name,omitempty"`
	// Large-file chunking (>700KB goes through chunked transfer to stay under the relay's 1MB per-envelope cap).
	// ≤700KB still takes the inline path above and these fields stay empty. When chunking: first send a
	// "chunk announcement" (mission_update with metadata but empty Artifact), then N artifact_chunk messages
	// in order (each with this chunk's base64 + the same metadata; self-contained, may arrive out of order).
	ArtifactID   string `json:"artifact_id,omitempty"`   // unique ID of one file transfer (reassembly key + msgID used when finally written to disk)
	ChunkIndex   int    `json:"chunk_index,omitempty"`   // index of this chunk (0..ChunkTotal-1)
	ChunkTotal   int    `json:"chunk_total,omitempty"`   // total number of chunks
	ArtifactSHA  string `json:"artifact_sha,omitempty"`  // sha256(hex) of the whole file, verified after reassembly
	ArtifactSize int64  `json:"artifact_size,omitempty"` // original size of the whole file in bytes (display / progress)
	// Task carries the mission summary when type="task" (read-only display; full data lives in MissionStore).
	Task *TaskSummary `json:"task,omitempty"`
	// Share carries the app-share notice when type="app_share" (grant/revoke + sharer tunnel URL).
	Share *AppShare `json:"share,omitempty"`
	// QuoteMission is the mission ID this [plain message] refers to (a small reference card when chatting about an ongoing mission).
	// Status messages (task/mission_update) link missions via Task.MissionID; plain text uses this field.
	// Source: the alter writes a [[task:missionID]] marker in the reply body; it is parsed out and removed from the body before sending.
	QuoteMission string `json:"quote_mission,omitempty"`
	// GID is set on every group message (fan-out inner messages have it; To is then empty),
	// and on pairwise group_invite / group_key / group_leave to name the group they concern.
	GID string `json:"gid,omitempty"`
	// Group carries the signed roster in group_invite (and lets the owner push roster updates).
	Group *GroupRoster `json:"group,omitempty"`
	// GKey carries the sender's chain key in group_key (the group is named by GID).
	GKey *GroupKeyDist `json:"gkey,omitempty"`
	// By is the provenance of a group post: "owner" (the human typed it) or "alter" (the
	// agent composed it); "" counts as owner. Self-declared like `auto`; both ends
	// enforce it against the group profile (§14.7 AllowSpeak).
	By string `json:"by,omitempty"`
	// Agent names WHICH of the sender's agents composed a by=alter post (one seat can
	// carry several: the alter plus named seat agents, e.g. "DevBot"). Display-only and
	// self-declared; governance reads By, never this. Empty on by=alter means the
	// sender's default alter.
	Agent string `json:"agent,omitempty"`
	// Voices carries a TypeGroupVoices announcement: the sender's seat-agent names
	// currently enabled in the group named by GID (so other members' composers can
	// autocomplete @<agent>). Metadata, never archived; receivers cap and sanitize it.
	Voices []string `json:"voices,omitempty"`
	// PinRemove unpins the pin with this message id (type group_pin with empty body).
	PinRemove string `json:"pin_remove,omitempty"`
}

// TaskSummary is the mission summary embedded in a message (for the frontend task card).
type TaskSummary struct {
	MissionID  string   `json:"mission_id"`
	Title      string   `json:"title,omitempty"`
	Goal       string   `json:"goal"`
	Acceptance []string `json:"acceptance"`
	Budget     int      `json:"budget"`
	Deadline   string   `json:"deadline,omitempty"`
	Status     string   `json:"status"`
}

// Envelope is the [outer envelope] — visible to the relay, carrying only what routing needs + an anti-forgery signature. The content is encrypted in Cipher.
type Envelope struct {
	V      int       `json:"v"`
	To     string    `json:"to"`   // recipient's public-key fingerprint (the relay buckets by it)
	From   string    `json:"from"` // sender's Ed25519 public key (base64; the relay verifies the signature with it)
	TS     time.Time `json:"ts"`
	Cipher string    `json:"cipher"` // encrypted inner envelope (base64)
	Sig    string    `json:"sig"`    // sender's signature over to+ts+cipher (base64)
	// FromXPub is used only for friend_request/accept and pairwise group_* messages: the peer may
	// not hold a card of ours and could not otherwise obtain our X25519 public key to decrypt, so
	// the envelope declares it (plaintext; only used to derive the decryption key, no loss of
	// confidentiality).
	FromXPub string `json:"from_xpub,omitempty"`
	// GID marks a GROUP envelope (§14): Cipher is sender-key encrypted (not ECDH), the signature
	// covers gid+ts+cipher (see VerifyGroupEnvelope), and To is stamped by the relay per fan-out
	// copy for routing only. Empty on pairwise envelopes.
	GID string `json:"gid,omitempty"`
}

// XPubFromB64 restores a base64 X25519 public key to an *ecdh.PublicKey.
func XPubFromB64(b64 string) (*ecdh.PublicKey, error) {
	raw, err := DecodeKey(b64)
	if err != nil {
		return nil, fmt.Errorf("X25519 公钥非法")
	}
	return ecdh.X25519().NewPublicKey(raw)
}

// envelopeSigningBytes is the canonical signing string of the outer envelope.
func envelopeSigningBytes(to string, ts time.Time, cipher string) []byte {
	return []byte("a2a-envelope-v2\n" + to + "\n" + ts.UTC().Format(time.RFC3339Nano) + "\n" + cipher)
}

// VerifyEnvelope checks the outer envelope signature (used by the relay — confirms this ciphertext really was sent by From, preventing forged deliveries).
func (e *Envelope) VerifyEnvelope() error {
	if e.V != 2 {
		return fmt.Errorf("信封版本不支持: %d", e.V)
	}
	fromPub, err := DecodeKey(e.From)
	if err != nil || len(fromPub) != ed25519.PublicKeySize {
		return fmt.Errorf("发件公钥非法")
	}
	sig, err := DecodeKey(e.Sig)
	if err != nil {
		return fmt.Errorf("签名非 base64")
	}
	if !ed25519.Verify(fromPub, envelopeSigningBytes(e.To, e.TS, e.Cipher), sig) {
		return fmt.Errorf("信封签名校验失败")
	}
	return nil
}

// Fingerprint derives a short fingerprint from an Ed25519 public key (routing address / mailbox bucket name / file-name safe).
func Fingerprint(edPub ed25519.PublicKey) string {
	sum := sha256.Sum256(edPub)
	return base64.RawURLEncoding.EncodeToString(sum[:16]) // 22 characters, URL/file-name safe
}

// ——— End-to-end encryption (X25519 ECDH → AES-256-GCM, standard library only) ———

// deriveKey derives the 32-byte symmetric key from the ECDH shared secret.
func deriveKey(shared []byte) []byte {
	k := sha256.Sum256(append([]byte("soulmirror-a2a-v2-aead\n"), shared...))
	return k[:]
}

// Seal encrypts the inner envelope with the peer's X25519 public key + our X25519 private key, producing the value stored in Envelope.Cipher.
func Seal(myXPriv *ecdh.PrivateKey, theirXPub *ecdh.PublicKey, msg *Message) (string, error) {
	shared, err := myXPriv.ECDH(theirXPub)
	if err != nil {
		return "", fmt.Errorf("ECDH 失败: %w", err)
	}
	plain, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveKey(shared))
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
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Open decrypts Cipher with our X25519 private key + the peer's X25519 public key, restoring the inner envelope.
func Open(myXPriv *ecdh.PrivateKey, theirXPub *ecdh.PublicKey, cipherB64 string) (*Message, error) {
	shared, err := myXPriv.ECDH(theirXPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH 失败: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return nil, fmt.Errorf("密文非 base64")
	}
	block, err := aes.NewCipher(deriveKey(shared))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("密文过短")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("解密失败（密钥不匹配或被篡改）")
	}
	var m Message
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ——— Cards (A2A v2 §3.2: trust anchor, exchanged out of band) ———

// Card is an alter's card: identity public key + encryption public key + inbound relays + self-declared nickname + capability declaration.
type Card struct {
	V       int      `json:"v"`
	EdPub   string   `json:"pk"`             // Ed25519 public key base64 (identity)
	XPub    string   `json:"xpk"`            // X25519 public key base64 (encryption)
	Proxies []string `json:"proxy"`          // relays where I receive mail (primary/backup)
	Name    string   `json:"name,omitempty"` // self-declared nickname
	DescURL string   `json:"desc,omitempty"` // ADP capability declaration URL (optional)
	Sig     string   `json:"sig,omitempty"`  // self-signature over the fields above
}

// Fingerprint returns the public-key fingerprint of the card.
func (c *Card) Fingerprint() (string, error) {
	pub, err := DecodeKey(c.EdPub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("名片公钥非法")
	}
	return Fingerprint(pub), nil
}

// XPublic returns the X25519 public key in the card (for encryption).
func (c *Card) XPublic() (*ecdh.PublicKey, error) {
	raw, err := DecodeKey(c.XPub)
	if err != nil {
		return nil, fmt.Errorf("名片加密公钥非法")
	}
	return ecdh.X25519().NewPublicKey(raw)
}

func (c *Card) signingBytes() []byte {
	// stable string: version + both public keys + relay list + nickname + descURL (excluding sig itself)
	return []byte(fmt.Sprintf("a2a-card-v%d\n%s\n%s\n%s\n%s\n%s",
		c.V, c.EdPub, c.XPub, strings.Join(c.Proxies, ","), c.Name, c.DescURL))
}

// Sign self-signs the card with the Ed25519 private key.
func (c *Card) Sign(priv ed25519.PrivateKey) {
	c.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, c.signingBytes()))
}

// Verify checks the card's self-signature (confirms the card was published by the key owner and not tampered with).
func (c *Card) Verify() error {
	pub, err := DecodeKey(c.EdPub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("名片公钥非法")
	}
	sig, err := DecodeKey(c.Sig)
	if err != nil {
		return fmt.Errorf("名片签名非 base64")
	}
	if !ed25519.Verify(pub, c.signingBytes(), sig) {
		return fmt.Errorf("名片签名校验失败")
	}
	return nil
}

// EncodeURI encodes the card as a soulmirror://card?... link (renderable as a QR code / copyable).
func (c *Card) EncodeURI() string {
	q := url.Values{}
	q.Set("v", fmt.Sprint(c.V))
	q.Set("pk", c.EdPub)
	q.Set("xpk", c.XPub)
	q.Set("proxy", strings.Join(c.Proxies, ","))
	if c.Name != "" {
		q.Set("name", c.Name)
	}
	if c.DescURL != "" {
		q.Set("desc", c.DescURL)
	}
	if c.Sig != "" {
		q.Set("sig", c.Sig)
	}
	return "soulmirror://card?" + q.Encode()
}

// ParseCard parses a soulmirror://card?... link into a Card and verifies the signature.
func ParseCard(uri string) (*Card, error) {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, "soulmirror://card?") {
		return nil, fmt.Errorf("不是有效的灵镜名片链接")
	}
	q, err := url.ParseQuery(strings.TrimPrefix(uri, "soulmirror://card?"))
	if err != nil {
		return nil, err
	}
	c := &Card{
		V:       2,
		EdPub:   q.Get("pk"),
		XPub:    q.Get("xpk"),
		Name:    q.Get("name"),
		DescURL: q.Get("desc"),
		Sig:     q.Get("sig"),
	}
	if p := q.Get("proxy"); p != "" {
		c.Proxies = strings.Split(p, ",")
	}
	if c.EdPub == "" || c.XPub == "" || len(c.Proxies) == 0 {
		return nil, fmt.Errorf("名片缺少必要字段（pk/xpk/proxy）")
	}
	if err := c.Verify(); err != nil {
		return nil, err
	}
	return c, nil
}

// ConvID canonicalizes a conversation id: the two fingerprints joined in lexical order, so both sides compute the same value.
func ConvID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "~" + b
}

// nicknameRe constrains the self-declared nickname (length/visibility only, lenient).
var nicknameRe = regexp.MustCompile(`^.{1,32}$`)

// ValidNickname checks that the nickname is non-empty and not too long.
func ValidNickname(s string) error {
	if !nicknameRe.MatchString(strings.TrimSpace(s)) {
		return fmt.Errorf("昵称须为 1-32 字符")
	}
	return nil
}

// ——— Inbox authentication (GET /mail, ack: prove ownership of the mailbox) ———

// Request-signing HTTP headers (keyed by public key, no longer by handle).
const (
	HeaderPub       = "X-A2A-Pub"       // our Ed25519 public key base64
	HeaderTimestamp = "X-A2A-Timestamp" // RFC3339
	HeaderSignature = "X-A2A-Signature" // signature over method+path+ts
)

// MaxClockSkew is the request-timestamp skew tolerated by the relay (anti-replay).
const MaxClockSkew = 5 * time.Minute

func reqSigningBytes(method, path, ts string) []byte {
	return []byte("a2a-req-v2\n" + method + "\n" + path + "\n" + ts)
}

// SignReq signs an inbox-type request.
func SignReq(priv ed25519.PrivateKey, method, path, ts string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, reqSigningBytes(method, path, ts)))
}

// VerifyReq checks the signature + timestamp skew of an inbox-type request and returns the caller's fingerprint.
func VerifyReq(pubB64, method, path, ts, sigB64 string) (fingerprint string, err error) {
	pub, err := DecodeKey(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("公钥非法")
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "", fmt.Errorf("时间戳须为 RFC3339")
	}
	if d := time.Since(t); d > MaxClockSkew || d < -MaxClockSkew {
		return "", fmt.Errorf("时间戳偏差过大（防重放）")
	}
	sig, err := DecodeKey(sigB64)
	if err != nil {
		return "", fmt.Errorf("签名非 base64")
	}
	if !ed25519.Verify(pub, reqSigningBytes(method, path, ts), sig) {
		return "", fmt.Errorf("签名校验失败")
	}
	return Fingerprint(pub), nil
}

// EncodeKey / DecodeKey: binary ↔ base64.
func EncodeKey(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// DecodeKey decodes the raw bytes.
func DecodeKey(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
