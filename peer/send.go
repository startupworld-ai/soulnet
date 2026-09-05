package peer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// MaxSendFileBytes caps the raw size of an attachment we send (the chunking mechanism has
// no hard limit itself; this guards against accidents).
const MaxSendFileBytes = 50 << 20

// ErrQueued: the relay could not be reached, so the sealed envelope was parked in the
// outbox and will be re-sent by the Run loop. The message is NOT lost, but it has not
// been delivered either - a host should show it as "queued", never as "sent".
// Test with errors.Is.
var ErrQueued = fmt.Errorf("relay unreachable; queued in the outbox for retry")

// SendResult is the outcome of one Send.
type SendResult struct {
	ID     string `json:"id"`
	Seq    int    `json:"seq"`              // line number in the local conversation (0 when not archived)
	Status string `json:"status"`           // sent | queued (relay unreachable; queued in the outbox for automatic retry)
	Chunks int    `json:"chunks,omitempty"` // >0 means the attachment was chunked
}

// Attachment is one file to send along with a message: inline (base64 in the message)
// when it fits a2a.MaxArtifactBytes, otherwise the message becomes the chunk announcement
// and the bytes follow as artifact_chunk messages. It is also kept locally under
// ArtifactPath so the sender's own UI can offer it for download.
type Attachment struct {
	Name string
	Raw  []byte
}

// MessageOptions are the optional parts of one SendMessage.
type MessageOptions struct {
	// Attachment is an in-memory file to send with the message ("" name = none).
	Attachment *Attachment
	// File is the path of a local file to attach instead of Attachment (validated and
	// read with LoadSendFile).
	File string
	// Archive appends the message to the local conversation with the peer (Dir=out,
	// Status sent|queued, base64 stripped) and reports its Seq. Handshake and control
	// messages are sent without it.
	Archive bool
	// SessionID is stored with the archived copy (the host's session record for the
	// turn that produced this message).
	SessionID string
}

// SendMessage sends one arbitrary pairwise message (any inner type, including types only
// the host understands) to a card: it completes From / To / ID / TS (and ConvID when
// archiving), runs BeforeSend, seals, delivers or queues, runs AfterSend, optionally
// attaches a file and optionally archives. Returns the result plus:
//   - nil            — delivered;
//   - ErrQueued      — parked in the outbox (res.Status == "queued", res still valid);
//   - any other error — nothing was sent (bad file, no identity, cannot even queue).
//
// The receiver need not be a friend (a card is enough); friendship rules are the
// receiver's business.
func (n *Peer) SendMessage(ctx context.Context, to *a2a.Card, msg *a2a.Message, opts MessageOptions) (*SendResult, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	if to == nil || msg == nil {
		return nil, fmt.Errorf("%w: card and message are required", ErrBadCard)
	}
	toFp, err := to.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	ctx = ctxOrBackground(ctx)
	att := opts.Attachment
	if strings.TrimSpace(opts.File) != "" {
		name, raw, err := LoadSendFile(opts.File, MaxSendFileBytes)
		if err != nil {
			return nil, err
		}
		att = &Attachment{Name: name, Raw: raw}
	}
	n.completeMessage(toFp, msg)
	if opts.Archive && msg.ConvID == "" {
		msg.ConvID = a2a.ConvID(msg.From, toFp)
	}
	res := &SendResult{ID: msg.ID, Status: "sent"}
	var sendErr error
	switch {
	case att != nil && len(att.Raw) > 0 && a2a.ShouldChunk(len(att.Raw)):
		res.Chunks, sendErr = n.sendChunked(ctx, to, toFp, msg, att.Raw, att.Name)
	default:
		if att != nil && len(att.Raw) > 0 {
			msg.ArtifactName = att.Name
			msg.Artifact = base64.StdEncoding.EncodeToString(att.Raw)
			n.PersistArtifactBytes(toFp, msg.ID, att.Name, att.Raw)
		}
		sendErr = n.sendMessage(ctx, to, msg)
	}
	if sendErr != nil {
		if !errors.Is(sendErr, ErrQueued) {
			return nil, sendErr
		}
		res.Status = "queued"
	}
	if opts.Archive {
		stored := *msg
		stored.Artifact = ""
		seq, err := n.Convs.AppendSeq(toFp, &a2a.ConvEntry{Dir: "out", Message: stored, Status: res.Status, SessionID: opts.SessionID})
		if err != nil {
			return nil, err
		}
		res.Seq = seq
	}
	return res, sendErr
}

// completeMessage fills the identity fields a caller may leave empty: From (me), To, a
// fresh ID and TS. A message with an empty From would be dropped by every receiver as
// "not a friend" - a bug hosts have hit when sending status messages.
func (n *Peer) completeMessage(toFp string, msg *a2a.Message) {
	if msg.From == "" {
		msg.From = n.Fingerprint()
	}
	if msg.To == "" {
		msg.To = toFp
	}
	if msg.ID == "" {
		msg.ID = n.newMsgID()
	}
	if msg.TS.IsZero() {
		msg.TS = time.Now()
	}
}

// SendOptions are the optional parts of one Send.
type SendOptions struct {
	// File is the path of one local file to attach ("" = none).
	File string
	// Auto marks the message as an automatic reply produced by the host's alter (A2A
	// `auto` flag, wire spec §"Message"). Receivers use it as the loop guard: an auto
	// message must never trigger another automatic reply.
	Auto bool
}

// Send sends a text to a friend, optionally with one local file attached (filePath "" =
// none). Shorthand for SendWith with only the file option.
func (n *Peer) Send(ctx context.Context, to, body, filePath string) (*SendResult, error) {
	return n.SendWith(ctx, to, body, SendOptions{File: filePath})
}

// SendWith sends a text to a friend with the given options.
//   - Not a friend: ErrNotFriend (AddFriend first).
//   - Attachment ≤ a2a.MaxArtifactBytes: base64 inline with the message; larger: this
//     message becomes the "chunk announcement", followed by artifact_chunk messages.
//   - Relay unreachable: queued in the outbox and re-sent by the Run loop; Status=queued
//     (no error - the message is archived and will go out).
//   - opts.Auto: the message (and its archived copy) carries the A2A `auto` flag.
func (n *Peer) SendWith(ctx context.Context, to, body string, opts SendOptions) (*SendResult, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	fr := n.Friends.Get(to)
	if fr == nil || fr.Card == nil {
		return nil, ErrNotFriend
	}
	body = strings.TrimSpace(body)
	if body == "" && strings.TrimSpace(opts.File) == "" {
		return nil, fmt.Errorf("%w: body and attachment cannot both be empty", ErrBadFile)
	}
	msg := &a2a.Message{Type: a2a.TypeText, Body: body, Auto: opts.Auto}
	res, err := n.SendMessage(ctx, fr.Card, msg, MessageOptions{File: opts.File, Archive: true})
	if err != nil && !errors.Is(err, ErrQueued) {
		return nil, err
	}
	return res, nil
}

// Typing sends a "busy" on/off signal to a friend (best effort: failures are silent, not
// queued, not archived).
func (n *Peer) Typing(ctx context.Context, to string, on bool) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	fr := n.Friends.Get(to)
	if fr == nil || fr.Card == nil {
		return ErrNotFriend
	}
	state := "on"
	if !on {
		state = "off"
	}
	msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: fr.Fingerprint,
		TS: time.Now(), Type: a2a.TypeTyping, Body: state}
	env, err := n.seal(fr.Card, msg)
	if err != nil {
		return err
	}
	return n.DeliverToCard(ctxOrBackground(ctx), fr.Card, env)
}

// sendChunked sends a large file in chunks: the announcement (this message carries the
// metadata, no bytes) + N artifact_chunk messages; the whole file is also kept locally.
// Returns the chunk count; the first delivery failure is returned (that message is queued).
func (n *Peer) sendChunked(ctx context.Context, to *a2a.Card, toFp string, announce *a2a.Message, raw []byte, name string) (int, error) {
	artifactID := a2a.NewArtifactID()
	chunks := a2a.SplitChunks(raw)
	total := len(chunks)
	sha := a2a.SHA256Hex(raw)
	size := int64(len(raw))

	announce.ArtifactName = name
	announce.ArtifactID = artifactID
	announce.ChunkTotal = total
	announce.ArtifactSHA = sha
	announce.ArtifactSize = size
	announce.Artifact = ""
	n.PersistArtifactBytes(toFp, artifactID, name, raw)

	firstErr := n.sendMessage(ctx, to, announce)
	for i, c := range chunks {
		chunk := &a2a.Message{
			ID: n.newMsgID(), From: announce.From, To: announce.To, TS: time.Now(),
			Type:         a2a.TypeArtifactChunk,
			ArtifactID:   artifactID,
			ArtifactName: name,
			ChunkIndex:   i,
			ChunkTotal:   total,
			ArtifactSHA:  sha,
			ArtifactSize: size,
			Artifact:     base64.StdEncoding.EncodeToString(c),
		}
		if err := n.sendMessage(ctx, to, chunk); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	n.logf("large file %s sent to %s in %d chunks (%d KB)", name, a2a.ShortFp(toFp), total, size/1024)
	return total, firstErr
}

// sendMessage is the single pairwise send path: complete identity fields → BeforeSend →
// seal (the handshake rule, from_xpub on friend_request / friend_accept, lives in
// a2a.SealEnvelope) → deliver to the relays in the card → on failure park the envelope
// in the outbox and return ErrQueued → AfterSend.
func (n *Peer) sendMessage(ctx context.Context, toCard *a2a.Card, msg *a2a.Message) error {
	toFp, _ := toCard.Fingerprint()
	n.completeMessage(toFp, msg)
	if n.BeforeSend != nil {
		n.BeforeSend(toCard, msg)
	}
	env, err := n.seal(toCard, msg)
	if err != nil {
		return err
	}
	if err := n.DeliverToCard(ctx, toCard, env); err != nil {
		n.logf("delivery failed (queued for retry): %v", err)
		if qerr := n.queueOutbox(toCard, env); qerr != nil {
			return fmt.Errorf("delivery failed and could not be queued: %v / %v", err, qerr)
		}
		if n.AfterSend != nil {
			n.AfterSend(toCard, msg, true)
		}
		return fmt.Errorf("%w: %v", ErrQueued, err)
	}
	n.logf("<<< mail to=%s type=%s id=%s", a2a.ShortFp(toFp), msg.Type, msg.ID)
	if n.AfterSend != nil {
		n.AfterSend(toCard, msg, false)
	}
	return nil
}

// seal encrypts + signs the envelope. a2a.SealEnvelope applies the handshake rule
// itself (friend_request / friend_accept carry from_xpub), so nothing to patch here.
func (n *Peer) seal(toCard *a2a.Card, msg *a2a.Message) (*a2a.Envelope, error) {
	id := n.Identity()
	if id == nil {
		return nil, ErrNoIdentity
	}
	return a2a.SealEnvelope(id, toCard, msg)
}

// DeliverTimeout is the HTTP timeout for short relay requests (deliver / ack / presence).
// The long-poll client keeps a2a.DefaultPollTimeout; a delivery must not wait that long
// or Send would stall the host whenever a relay is down.
const DeliverTimeout = a2a.DefaultDeliverTimeout

// DeliverToCard tries each relay in the card in order (short DeliverTimeout per attempt).
// Any failure is wrapped in ErrNetwork so the host can map it to its network error code.
func (n *Peer) DeliverToCard(ctx context.Context, card *a2a.Card, env *a2a.Envelope) error {
	id := n.Identity()
	var lastErr error
	for _, proxy := range card.Proxies {
		pc := a2a.NewProxyClient(proxy, id).WithDeliverTimeout(DeliverTimeout)
		if err := pc.Deliver(ctx, env); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		return fmt.Errorf("%w: the peer's card lists no usable relay", ErrNetwork)
	}
	return fmt.Errorf("%w: %v", ErrNetwork, lastErr)
}

// ——— outbox (shared format, a2a.WriteOutbox/ReadOutbox: a2a/outbox/<ns>-<seq>.json = {card, env}) ———

func (n *Peer) outboxDir() string { return a2a.OutboxDir(n.Home) }

func (n *Peer) queueOutbox(card *a2a.Card, env *a2a.Envelope) error {
	_, err := a2a.WriteOutbox(n.outboxDir(), &a2a.OutboxItem{Card: card, Env: env})
	return err
}

// flushOutbox replays queued envelopes in file order; stops at the first one that still
// cannot be delivered (retry next round). Malformed files are dropped. Returns how many
// were re-sent.
func (n *Peer) flushOutbox(ctx context.Context) int {
	entries, err := a2a.ReadOutbox(n.outboxDir())
	if err != nil {
		return 0
	}
	sent := 0
	for _, e := range entries {
		if e.Err != nil {
			n.logf("dropping malformed outbox file %s: %v", e.Name, e.Err)
			_ = a2a.RemoveOutbox(n.outboxDir(), e.Name)
			continue
		}
		if err := n.DeliverToCard(ctx, e.Item.Card, e.Item.Env); err != nil {
			return sent
		}
		_ = a2a.RemoveOutbox(n.outboxDir(), e.Name)
		sent++
		n.heartbeat(HeartbeatProgress)
	}
	if sent > 0 {
		n.logf("outbox: re-sent %d queued envelope(s)", sent)
	}
	return sent
}

// OutboxLen returns the number of queued files (diagnostics).
func (n *Peer) OutboxLen() int {
	entries, err := a2a.ReadOutbox(n.outboxDir())
	if err != nil {
		return 0
	}
	return len(entries)
}

// ——— attachments on disk (same layout as SoulMirror: a2a/artifacts/<peer>/<msgID or artifactID>__<name>) ———

// ArtifactPath is where an attachment lives on disk: a2a/artifacts/<peer>/<key>__<name>,
// key being the message id (inline attachment) or the artifact id (chunked transfer).
func (n *Peer) ArtifactPath(peer, key, name string) string {
	return filepath.Join(n.Home, "a2a", "artifacts", a2a.SanitizeID(peer), a2a.SanitizeID(key)+"__"+name)
}

// ArtifactFile returns the on-disk path of an attachment (name must not contain path
// separators). Errors when it does not exist.
func (n *Peer) ArtifactFile(peer, key, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid attachment name")
	}
	p := n.ArtifactPath(peer, key, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("attachment not found")
	}
	return p, nil
}

// PersistArtifactBytes writes an attachment to ArtifactPath(peer, key, name) and returns
// the path ("" when writing failed; the failure is logged).
func (n *Peer) PersistArtifactBytes(peer, key, name string, raw []byte) string {
	p := n.ArtifactPath(peer, key, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		n.logf("writing attachment failed: %v", err)
		return ""
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		n.logf("writing attachment failed: %v", err)
		return ""
	}
	return p
}

// LoadSendFile validates and reads an attachment to send: exists, non-empty, regular
// file, within limit bytes, file name without path separators. Every failure wraps
// ErrBadFile (size overflow additionally wraps ErrArtifactSize).
func LoadSendFile(path string, limit int64) (name string, raw []byte, err error) {
	info, serr := os.Stat(path)
	if serr != nil {
		return "", nil, fmt.Errorf("%w: not readable: %v", ErrBadFile, serr)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%w: not a regular file: %s", ErrBadFile, path)
	}
	if info.Size() == 0 {
		return "", nil, fmt.Errorf("%w: empty file", ErrBadFile)
	}
	if info.Size() > limit {
		return "", nil, fmt.Errorf("%w (%d bytes > %d)", ErrArtifactSize, info.Size(), limit)
	}
	name = filepath.Base(path)
	if name == "" || name == "." || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", nil, fmt.Errorf("%w: invalid file name", ErrBadFile)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrBadFile, err)
	}
	return name, raw, nil
}
