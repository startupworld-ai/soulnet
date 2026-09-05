package peer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// FriendSummary is one row of the friend list: the friend plus a conversation summary.
type FriendSummary struct {
	*a2a.Friend
	Count  int            `json:"count"`            // total conversation entries
	Unread int            `json:"unread"`           // unread (sent by the peer after the read cursor)
	Last   *a2a.ConvEntry `json:"last,omitempty"`   // last entry (artifact base64 stripped)
	Typing bool           `json:"typing,omitempty"` // the peer is "busy" right now
}

// FriendList returns the friends with their conversation summaries.
func (n *Peer) FriendList() []FriendSummary {
	frs := n.Friends.Friends()
	out := make([]FriendSummary, 0, len(frs))
	for _, fr := range frs {
		cnt, last, unread := n.Convs.Summary(fr.Fingerprint, fr.LastReadAt)
		if last != nil {
			cp := *last
			cp.Artifact = ""
			last = &cp
		}
		out = append(out, FriendSummary{Friend: fr, Count: cnt, Unread: unread, Last: last, Typing: n.PeerTyping(fr.Fingerprint)})
	}
	return out
}

// PendingRequests returns the pending friend requests in creation order (never nil;
// a2a.PendingStore.List already guarantees "[]" for JSON).
func (n *Peer) PendingRequests() []*a2a.Pending { return n.Pendings.List() }

// AddFriend sends a friend request from the peer's card link: verify the card → create the
// local entry first (keeps the card so we can encrypt) → send friend_request. note is both
// the local note and the request greeting. Once the peer accepts, a friend_accept comes back
// (→ EventFriendAccepted). Returns the local friend entry; when sending fails (relay
// unreachable) the request is queued in the outbox for retry.
func (n *Peer) AddFriend(ctx context.Context, cardURI, note string) (*a2a.Friend, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	card, err := a2a.ParseCard(cardURI)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	return n.AddFriendCard(ctx, card, note)
}

// AddFriendCard is AddFriend with an already parsed card.
func (n *Peer) AddFriendCard(ctx context.Context, card *a2a.Card, note string) (*a2a.Friend, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	if err := card.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	toFp, err := card.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	if toFp == n.Fingerprint() {
		return nil, ErrSelf
	}
	myCard, err := n.Card()
	if err != nil {
		return nil, err
	}
	if err := n.Friends.Add(card, note); err != nil {
		return nil, err
	}
	msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: toFp, TS: time.Now(),
		Type: a2a.TypeFriendRequest, Body: strings.TrimSpace(note), Card: myCard}
	if err := n.sendMessage(ctxOrBackground(ctx), card, msg); err != nil {
		return nil, err
	}
	n.logf("friend request sent to %s", a2a.ShortFp(toFp))
	return n.Friends.Get(toFp), nil
}

// Accept accepts a pending friend request: create the entry (empty note → the peer's name)
// → reply friend_accept → delete the pending item.
func (n *Peer) Accept(ctx context.Context, pendingID, note string) (*a2a.Friend, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	p := n.Pendings.Get(pendingID)
	if p == nil {
		return nil, ErrNoPending
	}
	card := p.Incoming.Card
	if card == nil {
		_ = n.Pendings.Delete(pendingID)
		return nil, fmt.Errorf("%w: request carries no card", ErrBadCard)
	}
	toFp, err := card.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	if err := n.Friends.Add(card, note); err != nil {
		return nil, err
	}
	go n.SyncFriendProfile(toFp)
	myCard, err := n.Card()
	if err != nil {
		return nil, err
	}
	msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: toFp, TS: time.Now(),
		Type: a2a.TypeFriendAccept, Card: myCard}
	_ = n.Pendings.Delete(pendingID)
	if err := n.sendMessage(ctxOrBackground(ctx), card, msg); err != nil {
		return nil, err
	}
	n.logf("accepted friend request from %s", a2a.ShortFp(toFp))
	return n.Friends.Get(toFp), nil
}

// RemoveFriend deletes fp from friends.yaml (local only, the peer is not notified; the
// conversation archive and artifacts stay on disk). Returns ErrNotFriend when fp is not
// a friend. Presence / typing caches for fp are cleared.
func (n *Peer) RemoveFriend(fp string) error {
	if n.Friends.Get(fp) == nil {
		return ErrNotFriend
	}
	if err := n.Friends.Remove(fp); err != nil {
		return err
	}
	n.setPeerTyping(fp, false)
	n.presMu.Lock()
	delete(n.presCac, fp)
	n.presMu.Unlock()
	n.logf("removed friend %s", a2a.ShortFp(fp))
	return nil
}

// Reject rejects (deletes) a pending request. The peer is not notified (silent, same as
// SoulMirror).
func (n *Peer) Reject(pendingID string) error {
	if n.Pendings.Get(pendingID) == nil {
		return ErrNoPending
	}
	return n.Pendings.Delete(pendingID)
}

// SetFriend changes the note / per-friend protocol of a friend (protocol nil = untouched).
func (n *Peer) SetFriend(fp, note string, protocol *string) (*a2a.Friend, error) {
	if n.Friends.Get(fp) == nil {
		return nil, ErrNotFriend
	}
	if note != "" {
		if err := n.Friends.SetNote(fp, note); err != nil {
			return nil, err
		}
	}
	if protocol != nil {
		if err := n.Friends.SetProtocol(fp, *protocol); err != nil {
			return nil, err
		}
	}
	return n.Friends.Get(fp), nil
}

// Entry is one conversation line (artifact base64 stripped) plus its 1-based line number
// in messages.jsonl. It is the shared a2a.SeqEntry; JSON shape {"seq":n, …ConvEntry…}.
type Entry = a2a.SeqEntry

// Conversation returns the entries of the conversation with fp whose seq > sinceSeq, in
// order. Mechanism records (artifact_chunk dedupe lines) are filtered out but keep their
// line number, so seq may have gaps. limit > 0 keeps only the last limit entries. Never
// returns nil. Reads incrementally via a2a.ConvStore.Since (lines ≤ sinceSeq are not
// decoded).
func (n *Peer) Conversation(fp string, sinceSeq, limit int) []Entry {
	raw := n.Convs.Since(fp, sinceSeq, 0)
	out := make([]Entry, 0, len(raw))
	for _, e := range raw {
		if e.Type == a2a.TypeArtifactChunk {
			continue
		}
		e.Artifact = ""
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// MarkRead moves the read cursor for fp to the timestamp of line seq; seq <= 0 or beyond
// the end means "now" (everything read).
func (n *Peer) MarkRead(fp string, seq int) error {
	if n.Friends.Get(fp) == nil {
		return ErrNotFriend
	}
	t := time.Now()
	if seq > 0 {
		if es := n.Convs.Since(fp, seq-1, 0); len(es) > 0 && es[0].Seq == seq {
			t = es[0].TS
		}
	}
	return n.Friends.MarkRead(fp, t)
}

// SyncFriendProfile fetches the peer's full capability profile from the directory (after
// befriending, or on demand when a host needs it and the local copy is missing) and stores
// it at profiles/<fp>.json. Card and profile signatures and the fingerprint must agree.
// Not found / verification failure / directory unreachable all degrade to nil (logged).
func (n *Peer) SyncFriendProfile(fp string) *a2a.Profile {
	if fp == "" {
		return nil
	}
	hit, err := a2a.FetchProfile(n.RelayBase(), fp)
	if err != nil || hit == nil || hit.Profile == nil || hit.Card == nil {
		return nil
	}
	if err := hit.Card.Verify(); err != nil {
		n.logf("profile sync for %s: card verification failed (ignored): %v", a2a.ShortFp(fp), err)
		return nil
	}
	if err := hit.Profile.Verify(hit.Card.EdPub); err != nil {
		n.logf("profile sync for %s: profile verification failed (ignored): %v", a2a.ShortFp(fp), err)
		return nil
	}
	if cfp, e := hit.Card.Fingerprint(); e != nil || cfp != fp || hit.Profile.Fingerprint != fp {
		n.logf("profile sync for %s: fingerprint mismatch (ignored)", a2a.ShortFp(fp))
		return nil
	}
	if err := n.Profiles.SaveFriend(fp, hit.Profile); err != nil {
		n.logf("profile sync for %s: write failed (ignored): %v", a2a.ShortFp(fp), err)
		return nil
	}
	return hit.Profile
}
