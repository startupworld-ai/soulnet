package peer

import (
	"context"
	"fmt"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// not_friend bounce: a friend who has since dropped us (deleted us, reinstalled, lost its
// data) used to discard our mail silently, while our side still showed them as a friend
// and every message, file and group invite as sent. Now the receiver opens such mail (the
// sender's key rides on every envelope, FromXPub) and answers with a small not_friend; the
// sender's peer turns it into EventNotFriend so the host can say so and offer to re-add.
//
// Only mail that would otherwise be dropped is bounced (text / app_share / chunk / group
// invite from a non-friend); handshakes, group pairwise mechanics from co-members and
// not_friend itself never are, so two sides that both dropped each other cannot loop.

const (
	notFriendPerHour   = 30  // bounces per stranger per hour (a flood of dropped mail stays bounded)
	notFriendCardLimit = 256 // remembered stranger keys before the cache is reset
)

// rememberStranger keeps the key a non-friend declared on its envelope, so a dropped
// message can be answered.
func (n *Peer) rememberStranger(edPubB64, xPubB64 string) {
	fp := fingerprintOfB64(edPubB64)
	if fp == "" || xPubB64 == "" {
		return
	}
	n.nfMu.Lock()
	defer n.nfMu.Unlock()
	if n.nfCard == nil || len(n.nfCard) >= notFriendCardLimit {
		n.nfCard = map[string]*a2a.Card{}
	}
	n.nfCard[fp] = &a2a.Card{V: 2, EdPub: edPubB64, XPub: xPubB64}
}

// bounceNotFriend tells the sender of a dropped message that I do not hold them as a
// friend. Best effort, asynchronous, never queued; rate-limited per sender.
func (n *Peer) bounceNotFriend(msg *a2a.Message) {
	if msg == nil || msg.Type == a2a.TypeNotFriend || a2a.IsHandshake(msg.Type) || !n.HasIdentity() {
		return
	}
	from := msg.From
	now := time.Now()
	n.nfMu.Lock()
	card := n.nfCard[from]
	if card == nil {
		n.nfMu.Unlock()
		return // the sender declared no key (an older client): nothing to encrypt to
	}
	if n.nfSent == nil {
		n.nfSent = map[string][]time.Time{}
	}
	recent := n.nfSent[from][:0]
	for _, t := range n.nfSent[from] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= notFriendPerHour {
		n.nfSent[from] = recent
		n.nfMu.Unlock()
		return
	}
	n.nfSent[from] = append(recent, now)
	to := *card
	n.nfMu.Unlock()
	to.Proxies = []string{n.RelayBase()}
	b := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: from, TS: now,
		Type: a2a.TypeNotFriend, Body: msg.ID, GID: msg.GID}
	go func() {
		env, err := n.seal(&to, b)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*DeliverTimeout)
		defer cancel()
		if err := n.DeliverToCard(ctx, &to, env); err != nil {
			n.logf("not_friend bounce to %s failed: %v", a2a.ShortFp(from), err)
			return
		}
		n.logf("bounced %s %s from non-friend %s (not_friend)", msg.Type, msg.ID, a2a.ShortFp(from))
	}()
}

// handleNotFriend: a friend says it no longer holds me as a friend. Only a current friend's
// bounce means anything (a stranger saying so is the normal state); not archived.
func (n *Peer) handleNotFriend(msg *a2a.Message) error {
	if !n.Friends.IsFriend(msg.From) {
		return nil
	}
	n.logf("%s no longer holds me as a friend (dropped %s)", a2a.ShortFp(msg.From), msg.Body)
	n.emit(Event{Kind: EventNotFriend, Peer: msg.From, GID: msg.GID, TS: time.Now(), Message: msg})
	return nil
}

// GroupResendInvite sends the current roster again to a member whose invite was lost --
// typically one that bounced not_friend and has since re-added us. Owner or admin only;
// idempotent on the receiving side (an existing group just applies the roster).
func (n *Peer) GroupResendInvite(ctx context.Context, gid, fp string) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	me := n.Fingerprint()
	if st.Roster.OwnerFp() != me && !st.Roster.Profile.IsAdmin(me) {
		return fmt.Errorf("%w: only the owner or an admin can invite", ErrGroupOwner)
	}
	card := st.Roster.Member(fp)
	if card == nil || fp == me {
		return fmt.Errorf("%s is not a member of group %s", a2a.ShortFp(fp), a2a.ShortFp(gid))
	}
	invite := &a2a.Message{ID: n.newMsgID(), From: me, To: fp, TS: time.Now(),
		Type: a2a.TypeGroupInvite, GID: gid, Group: st.Roster, Body: st.Roster.Name}
	return n.sendGroupPairwise(ctxOrBackground(ctx), card, invite)
}
