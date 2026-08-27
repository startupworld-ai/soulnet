package peer

import (
	"context"
	"encoding/base64"
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

// SendResult is the outcome of one Send.
type SendResult struct {
	ID     string `json:"id"`
	Seq    int    `json:"seq"`              // line number in the local conversation
	Status string `json:"status"`           // sent | queued (relay unreachable; queued in the outbox for automatic retry)
	Chunks int    `json:"chunks,omitempty"` // >0 means the attachment was chunked
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
//   - Relay unreachable: queued in the outbox and re-sent by the Run loop; Status=queued.
//   - opts.Auto: the message (and its archived copy) carries the A2A `auto` flag.
func (n *Peer) SendWith(ctx context.Context, to, body string, opts SendOptions) (*SendResult, error) {
	filePath := opts.File
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	fr := n.Friends.Get(to)
	if fr == nil || fr.Card == nil {
		return nil, ErrNotFriend
	}
	ctx = ctxOrBackground(ctx)
	body = strings.TrimSpace(body)
	var (
		artName string
		artRaw  []byte
	)
	if strings.TrimSpace(filePath) != "" {
		name, raw, err := loadSendFile(filePath, MaxSendFileBytes)
		if err != nil {
			return nil, err
		}
		artName, artRaw = name, raw
	}
	if body == "" && len(artRaw) == 0 {
		return nil, fmt.Errorf("%w: body and attachment cannot both be empty", ErrBadFile)
	}
	me := n.Fingerprint()
	msg := &a2a.Message{ID: n.newMsgID(), From: me, To: fr.Fingerprint,
		ConvID: a2a.ConvID(me, fr.Fingerprint), TS: time.Now(), Type: a2a.TypeText, Body: body, Auto: opts.Auto}
	res := &SendResult{ID: msg.ID, Status: "sent"}

	if len(artRaw) > 0 && a2a.ShouldChunk(len(artRaw)) {
		chunks, err := n.sendChunked(ctx, fr, msg, artRaw, artName)
		res.Chunks = chunks
		if err != nil {
			res.Status = "queued"
		}
	} else {
		if len(artRaw) > 0 {
			msg.ArtifactName = artName
			msg.Artifact = base64.StdEncoding.EncodeToString(artRaw)
			n.persistArtifactBytes(fr.Fingerprint, msg.ID, artName, artRaw)
		}
		if err := n.sendMessage(ctx, fr.Card, msg); err != nil {
			res.Status = "queued"
		}
	}
	stored := *msg
	stored.Artifact = ""
	seq, err := n.Convs.AppendSeq(fr.Fingerprint, &a2a.ConvEntry{Dir: "out", Message: stored, Status: res.Status})
	if err != nil {
		return nil, err
	}
	res.Seq = seq
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
	return n.deliverToCard(ctxOrBackground(ctx), fr.Card, env)
}

// sendChunked sends a large file in chunks: the announcement (this text message carries
// the metadata, no bytes) + N artifact_chunk messages; the whole file is also kept locally.
// Returns the chunk count; any delivery failure returns an error (that message is queued).
func (n *Peer) sendChunked(ctx context.Context, fr *a2a.Friend, announce *a2a.Message, raw []byte, name string) (int, error) {
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
	n.persistArtifactBytes(fr.Fingerprint, artifactID, name, raw)

	firstErr := n.sendMessage(ctx, fr.Card, announce)
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
		if err := n.sendMessage(ctx, fr.Card, chunk); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	n.logf("large file %s sent to %s in %d chunks (%d KB)", name, a2a.ShortFp(fr.Fingerprint), total, size/1024)
	return total, firstErr
}

// sendMessage encrypts and delivers to the relays declared in the peer's card; on failure
// the envelope is queued in the outbox and the error is returned (callers mark "queued").
func (n *Peer) sendMessage(ctx context.Context, toCard *a2a.Card, msg *a2a.Message) error {
	env, err := n.seal(toCard, msg)
	if err != nil {
		return err
	}
	if err := n.deliverToCard(ctx, toCard, env); err != nil {
		n.logf("delivery failed (queued for retry): %v", err)
		if qerr := n.queueOutbox(toCard, env); qerr != nil {
			return fmt.Errorf("delivery failed and could not be queued: %v / %v", err, qerr)
		}
		return err
	}
	if fp, ferr := toCard.Fingerprint(); ferr == nil {
		n.logf("<<< mail to=%s type=%s id=%s", a2a.ShortFp(fp), msg.Type, msg.ID)
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

// deliverToCard tries each relay in the card in order. Any failure is wrapped in
// ErrNetwork so the host can map it to its network error code.
func (n *Peer) deliverToCard(ctx context.Context, card *a2a.Card, env *a2a.Envelope) error {
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
		if err := n.deliverToCard(ctx, e.Item.Card, e.Item.Env); err != nil {
			return sent
		}
		_ = a2a.RemoveOutbox(n.outboxDir(), e.Name)
		sent++
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

func (n *Peer) artifactPath(peer, key, name string) string {
	return filepath.Join(n.Home, "a2a", "artifacts", a2a.SanitizeID(peer), a2a.SanitizeID(key)+"__"+name)
}

// ArtifactFile returns the on-disk path of an attachment (name must not contain path
// separators). Errors when it does not exist.
func (n *Peer) ArtifactFile(peer, key, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid attachment name")
	}
	p := n.artifactPath(peer, key, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("attachment not found")
	}
	return p, nil
}

func (n *Peer) persistArtifactBytes(peer, key, name string, raw []byte) string {
	p := n.artifactPath(peer, key, name)
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

// loadSendFile validates and reads the attachment to send: exists, non-empty, regular
// file, within limit, file name without path separators. Every failure wraps ErrBadFile
// (size overflow additionally wraps ErrArtifactSize).
func loadSendFile(path string, limit int64) (name string, raw []byte, err error) {
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
