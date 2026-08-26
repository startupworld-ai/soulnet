package peer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// permanentErr marks "this mail itself is broken, retrying will not help": the receive
// loop acks (deletes) such mail instead of leaving it on the relay to be redelivered
// forever. Transient errors (disk write failures etc.) are not acked and retried next round.
type permanentErr struct{ err error }

func (e permanentErr) Error() string { return e.err.Error() }
func (e permanentErr) Unwrap() error { return e.err }
func permanent(err error) error      { return permanentErr{err: err} }
func isPermanent(err error) bool {
	var p permanentErr
	return errors.As(err, &p)
}

// shortAck truncates an ack id (the inbox file name) for readable log lines.
func shortAck(ack string) string {
	if len(ack) > 16 {
		return ack[len(ack)-16:]
	}
	return ack
}

// PollWaitSec is the wait of each long poll in seconds (the relay caps it at 55).
const PollWaitSec = 25

// Run is the receive loop: flush outbox → long-poll → decrypt and dispatch → ack. It blocks
// until ctx is cancelled. Only one Run per Peer at a time; a second call returns an error
// immediately. Without an identity it returns ErrNoIdentity.
func (n *Peer) Run(ctx context.Context) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	n.runMu.Lock()
	if n.running {
		n.runMu.Unlock()
		return fmt.Errorf("receive loop already running")
	}
	n.running = true
	n.runMu.Unlock()
	defer func() {
		n.runMu.Lock()
		n.running = false
		n.runMu.Unlock()
	}()

	if n.PresenceInterval > 0 {
		go n.presenceWatch(ctx)
	}
	n.logf("receive loop started · relay=%s · fingerprint=%s", n.RelayBase(), n.Fingerprint())
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		n.flushOutbox(ctx)
		pc := n.proxyClient()
		items, err := pc.Poll(ctx, PollWaitSec)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			n.logf("poll failed (%v), retrying in %s", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
		if len(items) > 0 {
			n.logf("[recv-debug] poll returned %d item(s)", len(items))
		}
		var acks []string
		hadTransient := false
		for i := range items {
			it := &items[i]
			err := n.handleEnvelope(&it.Envelope)
			if err == nil {
				n.logf("[recv-debug] handle OK gid=%s type=%s ack=%s", a2a.ShortFp(it.Envelope.GID), a2a.ShortFp(it.Envelope.From), shortAck(it.AckID))
				acks = append(acks, it.AckID)
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			if isPermanent(err) {
				n.logf("[recv-debug] handle PERMANENT gid=%s from=%s err=%v", a2a.ShortFp(it.Envelope.GID), a2a.ShortFp(it.Envelope.From), err)
				n.noteDeadLetter(it.AckID, err)
				acks = append(acks, it.AckID)
				continue
			}
			n.logf("[recv-debug] handle TRANSIENT gid=%s from=%s err=%v", a2a.ShortFp(it.Envelope.GID), a2a.ShortFp(it.Envelope.From), err)
			n.logf("transient failure handling incoming mail (retry next round): %v", err)
			hadTransient = true
		}
		if len(acks) > 0 {
			n.logf("[recv-debug] acking %d item(s)", len(acks))
		}
		if err := pc.Ack(ctx, acks); err != nil && ctx.Err() == nil {
			n.logf("ack failed (redelivered next round, deduplicated idempotently): %v", err)
		}
		if hadTransient {
			// Unacked transient mail makes the long poll return instantly (the box is not
			// empty) — pause briefly so a wait (e.g. a group invite or sender key still in
			// flight) does not hot-spin the relay. The pause applies even when OTHER mail
			// was acked this round: without it a busy mailbox re-polls immediately and one
			// waiting letter can burn its whole retry budget (see retryBudget, which
			// assumes >=500ms between attempts) before its prerequisite arrives.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// Running reports whether the receive loop is running.
func (n *Peer) Running() bool {
	n.runMu.Lock()
	defer n.runMu.Unlock()
	return n.running
}

func (n *Peer) noteDeadLetter(ackID string, err error) {
	n.dlMu.Lock()
	_, seen := n.dlSeen[ackID]
	if !seen {
		n.dlSeen[ackID] = struct{}{}
	}
	n.dlMu.Unlock()
	if !seen {
		n.logf("dropping unprocessable incoming mail (acked, no redelivery): %v", err)
	}
}

// handleEnvelope opens the outer envelope → inner message → dispatch.
func (n *Peer) handleEnvelope(env *a2a.Envelope) error {
	if env.GID != "" {
		return n.handleGroupEnvelope(env) // group fan-out letter (sender-key encrypted, §14)
	}
	id := n.Identity()
	xPriv, err := id.XPrivate()
	if err != nil {
		return err
	}
	var theirXB64 string
	if fr := n.friendByEdPub(env.From); fr != nil && fr.Card != nil {
		theirXB64 = fr.Card.XPub
	} else if env.FromXPub != "" {
		theirXB64 = env.FromXPub // first contact from a stranger (friend request) declares its encryption key in the envelope
	} else if c := n.Groups.FindMemberCard(fingerprintOfB64(env.From)); c != nil {
		theirXB64 = c.XPub // a group co-member who is not a friend (their card is on the roster)
	} else {
		return permanent(fmt.Errorf("cannot determine the sender's encryption key (not a friend and the envelope carries no xpub)"))
	}
	theirX, err := a2a.XPubFromB64(theirXB64)
	if err != nil {
		return permanent(err)
	}
	// OpenFrom re-verifies the outer signature (do not trust the relay) and rejects an
	// inner from that differs from the envelope signer (spec §13 #1).
	msg, err := a2a.OpenFrom(env, xPriv, theirX)
	if err != nil {
		return permanent(err)
	}
	return n.dispatch(msg)
}

func fingerprintOfB64(edPubB64 string) string {
	pub, err := a2a.DecodeKey(edPubB64)
	if err != nil || len(pub) != 32 {
		return ""
	}
	return a2a.Fingerprint(pub)
}

func (n *Peer) friendByEdPub(edPubB64 string) *a2a.Friend {
	fp := fingerprintOfB64(edPubB64)
	if fp == "" {
		return nil
	}
	return n.Friends.Get(fp)
}

func (n *Peer) dispatch(msg *a2a.Message) error {
	peer := msg.From
	if msg.Type == a2a.TypeTyping {
		if n.Friends.IsFriend(peer) {
			on := msg.Body != "off"
			n.setPeerTyping(peer, on)
			n.emit(Event{Kind: EventTyping, Peer: peer, TS: time.Now(), On: on})
		}
		return nil
	}
	n.setPeerTyping(peer, false)
	switch msg.Type {
	case a2a.TypeFriendRequest:
		return n.handleFriendRequest(msg)
	case a2a.TypeFriendAccept:
		return n.handleFriendAccept(msg)
	case a2a.TypeText, a2a.TypeAppShare:
		return n.archiveIncoming(msg, EventMessageReceived)
	case a2a.TypeTask, a2a.TypeMissionUpdate, a2a.TypeMissionBid:
		return n.archiveIncoming(msg, EventMissionUpdate)
	case a2a.TypeArtifactChunk:
		return n.handleArtifactChunk(msg)
	case a2a.TypeGroupInvite:
		return n.handleGroupInvite(msg)
	case a2a.TypeGroupKey:
		return n.handleGroupKey(msg)
	case a2a.TypeGroupLeave:
		return n.handleGroupLeave(msg)
	case a2a.TypeGroupUpdate:
		return n.handleGroupUpdatePairwise(msg)
	case a2a.TypeGroupJoin:
		return n.handleGroupJoin(msg)
	case a2a.TypeGroupAdmin:
		return n.handleGroupAdmin(msg)
	default:
		n.logf("unknown message type %q (ignored)", msg.Type)
		return nil
	}
}

func (n *Peer) handleFriendRequest(msg *a2a.Message) error {
	if msg.Card == nil {
		return permanent(fmt.Errorf("friend request carries no card"))
	}
	if err := msg.Card.Verify(); err != nil {
		return permanent(fmt.Errorf("friend request card verification failed: %v", err))
	}
	if fp, err := msg.Card.Fingerprint(); err != nil || fp != msg.From {
		return permanent(fmt.Errorf("friend request card does not match the sender"))
	}
	if msg.ID == "" {
		return permanent(fmt.Errorf("friend request has no id"))
	}
	p := &a2a.Pending{ID: msg.ID, Peer: msg.From, Incoming: *msg, CreatedAt: time.Now()}
	if err := n.Pendings.Put(p); err != nil {
		return err
	}
	n.logf("friend request received · %s(%s): %s", cardName(msg.Card), a2a.ShortFp(msg.From), msg.Body)
	n.emit(Event{Kind: EventFriendRequest, Peer: msg.From, TS: time.Now(), Message: msg, PendingID: msg.ID})
	return nil
}

func (n *Peer) handleFriendAccept(msg *a2a.Message) error {
	if msg.Card == nil {
		return permanent(fmt.Errorf("friend_accept carries no card"))
	}
	if err := msg.Card.Verify(); err != nil {
		return permanent(fmt.Errorf("friend_accept card verification failed: %v", err))
	}
	if fp, err := msg.Card.Fingerprint(); err != nil || fp != msg.From {
		return permanent(fmt.Errorf("friend_accept card does not match the sender"))
	}
	if err := n.Friends.Add(msg.Card, ""); err != nil {
		return err
	}
	go n.syncFriendProfile(msg.From)
	fr := n.Friends.Get(msg.From)
	n.logf("%s accepted the friend request", a2a.ShortFp(msg.From))
	n.emit(Event{Kind: EventFriendAccepted, Peer: msg.From, TS: time.Now(), Friend: fr})
	return nil
}

// archiveIncoming handles mail from a friend: dedupe → write inline attachment → archive
// (without base64) → notify.
func (n *Peer) archiveIncoming(msg *a2a.Message, kind string) error {
	peer := msg.From
	if !n.Friends.IsFriend(peer) {
		n.logf("dropping %s from non-friend %s", msg.Type, a2a.ShortFp(peer))
		return nil
	}
	if msg.ID == "" {
		return permanent(fmt.Errorf("incoming message has no id"))
	}
	if n.Convs.Seen(peer, msg.ID) {
		return nil
	}
	artPath := ""
	if msg.Artifact != "" && msg.ArtifactName != "" {
		if raw, err := base64.StdEncoding.DecodeString(msg.Artifact); err == nil {
			artPath = n.persistArtifactBytes(peer, msg.ID, msg.ArtifactName, raw)
		}
	}
	stored := *msg
	stored.Artifact = ""
	seq, err := n.Convs.AppendSeq(peer, &a2a.ConvEntry{Dir: "in", Message: stored})
	if err != nil {
		return err
	}
	n.emit(Event{Kind: kind, Peer: peer, TS: time.Now(), Message: &stored, Seq: seq, ArtifactPath: artPath})
	return nil
}

// ——— chunk reassembly (same layout as SoulMirror: artifacts/<peer>/.incoming/<artifactID>/<i>.part) ———

func (n *Peer) incomingDir(peer, artifactID string) string {
	return filepath.Join(n.Home, "a2a", "artifacts", a2a.SanitizeID(peer), ".incoming", a2a.SanitizeID(artifactID))
}

func (n *Peer) handleArtifactChunk(msg *a2a.Message) error {
	peer := msg.From
	if !n.Friends.IsFriend(peer) {
		n.logf("dropping chunk from non-friend %s", a2a.ShortFp(peer))
		return nil
	}
	if msg.ID == "" {
		return permanent(fmt.Errorf("chunk has no id"))
	}
	if n.Convs.Seen(peer, msg.ID) {
		return nil
	}
	if msg.ArtifactID == "" || msg.ChunkTotal <= 0 || msg.ChunkIndex < 0 || msg.ChunkIndex >= msg.ChunkTotal || msg.ArtifactName == "" {
		return permanent(fmt.Errorf("invalid chunk (artifactID=%q index=%d total=%d)", msg.ArtifactID, msg.ChunkIndex, msg.ChunkTotal))
	}
	if strings.ContainsAny(msg.ArtifactName, `/\`) || strings.Contains(msg.ArtifactName, "..") {
		return permanent(fmt.Errorf("invalid chunk attachment name"))
	}
	raw, err := base64.StdEncoding.DecodeString(msg.Artifact)
	if err != nil {
		return permanent(fmt.Errorf("chunk base64 decode failed"))
	}
	dir := n.incomingDir(peer, msg.ArtifactID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(msg.ChunkIndex)+".part"), raw, 0o644); err != nil {
		return err
	}
	// Record a tiny dedupe line in the conversation jsonl (Conversation() filters it out).
	_ = n.Convs.Append(peer, &a2a.ConvEntry{Dir: "in", Message: a2a.Message{
		ID: msg.ID, From: peer, To: msg.To, TS: msg.TS, Type: a2a.TypeArtifactChunk,
		ArtifactID: msg.ArtifactID, ChunkIndex: msg.ChunkIndex, ChunkTotal: msg.ChunkTotal}})
	if countParts(dir) < msg.ChunkTotal {
		return nil
	}
	return n.reassemble(peer, msg.ArtifactID, msg.ArtifactName, msg.ChunkTotal, msg.ArtifactSHA)
}

func countParts(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	k := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".part") {
			k++
		}
	}
	return k
}

// reassemble concatenates the parts in order → sha256 check → writes
// artifacts/<peer>/<artifactID>__<name> → removes the staging dir → notifies.
// On a sha mismatch the staging dir is kept for inspection and an error is returned
// (permanent: acked — redelivering the same chunk would not change the outcome, and the
// dedupe line prevents a re-trigger anyway; the log shows it).
func (n *Peer) reassemble(peer, artifactID, name string, total int, wantSHA string) error {
	dir := n.incomingDir(peer, artifactID)
	var buf []byte
	for i := 0; i < total; i++ {
		part, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(i)+".part"))
		if err != nil {
			return nil // missing part: reassemble when the next chunk arrives
		}
		buf = append(buf, part...)
	}
	if wantSHA != "" {
		if got := a2a.SHA256Hex(buf); got != wantSHA {
			n.logf("large file %s: sha256 mismatch after reassembly (want=%s got=%s), staging dir kept for inspection", name, wantSHA, got)
			return permanent(fmt.Errorf("chunk reassembly sha256 check failed"))
		}
	}
	p := n.persistArtifactBytes(peer, artifactID, name, buf)
	if p == "" {
		return fmt.Errorf("write after reassembly failed")
	}
	_ = os.RemoveAll(dir)
	n.logf("large file %s reassembled (%d chunks, %d KB)", name, total, len(buf)/1024)
	n.emit(Event{Kind: EventArtifactReady, Peer: peer, TS: time.Now(), ArtifactPath: p, ArtifactName: name, ArtifactID: artifactID})
	return nil
}

// ——— typing mark ———

// typingTTL: lifetime of the peer's "busy" mark (an alter may work for minutes; be generous).
const typingTTL = 20 * time.Minute

func (n *Peer) setPeerTyping(peer string, on bool) {
	n.typMu.Lock()
	defer n.typMu.Unlock()
	if on {
		n.typing[peer] = time.Now().Add(typingTTL)
	} else {
		delete(n.typing, peer)
	}
}

// PeerTyping reports whether the peer is "busy" right now.
func (n *Peer) PeerTyping(peer string) bool {
	n.typMu.Lock()
	defer n.typMu.Unlock()
	until, ok := n.typing[peer]
	return ok && time.Now().Before(until)
}

func cardName(c *a2a.Card) string {
	if c != nil && c.Name != "" {
		return c.Name
	}
	return "?"
}
