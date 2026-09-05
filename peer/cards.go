// Card refresh: how a renamed identity reaches the people who hold its old card.
//
// A rename does not change the identity (the fingerprint is derived from the public key
// alone); it only produces a re-signed card. Everybody else still holds the card they got
// when they befriended us or when the owner signed us into a roster, so the new name has
// to travel. The rule is lazy sync, not broadcast: the card rides on the next message we
// send anyway, and only to receivers known to hold an older version.
//
// Pairwise mail already does this through the host's BeforeSend hook. Group posts are one
// fan-out envelope for everyone, so the peer does it here:
//
//   - sender: GroupSend attaches my card to a text post while any co-member on the roster
//     has not received this version through this group yet (groups/<gid>/cardsync.json,
//     per member); a successful fan-out marks every current co-member as up to date. A
//     card that did not change is never sent again.
//   - receiver: a card on an accepted group post is taken only when it verifies and its
//     fingerprint is the envelope signer's (a member cannot plant somebody else's card).
//     It is kept as that member's announced card for this group (groups/<gid>/cards.json,
//     what the member views show first), refreshes the friend snapshot when the member is
//     a friend, and - on the owner's node - converges the roster: the member's card is
//     swapped, version+1, re-signed and republished, so members who are nobody's friend
//     follow too. The republish happens once per card version (the roster then already
//     carries it).
//
// Wire: only the existing Message.Card field, on the existing text post. Receivers that
// predate this file archive the card unread, like they always did.
package peer

import (
	"context"
	"fmt"
	"strings"

	"github.com/startupworld-ai/soulnet/a2a"
)

// attachGroupCard puts my current card on a group post when some co-member on the roster
// has not received this version through this group yet. Returns the attached signature
// ("" = nothing attached: everyone is up to date, or no card).
func (n *Peer) attachGroupCard(st *a2a.GroupState, msg *a2a.Message) string {
	if msg == nil || msg.Card != nil || msg.Type != a2a.TypeText {
		return ""
	}
	card, err := n.Card()
	if err != nil || card.Sig == "" {
		return ""
	}
	me := n.Fingerprint()
	gid := st.Roster.GroupID
	rec := n.Groups.CardSync(gid)
	stale := 0
	for _, fp := range st.Roster.MemberFps() {
		if fp != me && rec[fp] != card.Sig {
			stale++
		}
	}
	if stale == 0 {
		return ""
	}
	msg.Card = card
	n.logf("group %s: attaching my card to this post (%d member(s) hold an older version)", a2a.ShortFp(gid), stale)
	return card.Sig
}

// markGroupCardSent records that every current co-member received my card with
// signature sig through gid (called after the fan-out was accepted by the relay).
func (n *Peer) markGroupCardSent(st *a2a.GroupState, sig string) {
	if sig == "" {
		return
	}
	me := n.Fingerprint()
	fps := make([]string, 0, len(st.Roster.Members))
	for _, fp := range st.Roster.MemberFps() {
		if fp != me {
			fps = append(fps, fp)
		}
	}
	if err := n.Groups.MarkCardSynced(st.Roster.GroupID, fps, sig); err != nil {
		n.logf("group %s: recording card sync failed (the card rides on the next post again, harmless): %v", a2a.ShortFp(st.Roster.GroupID), err)
	}
}

// absorbGroupCard consumes a card a co-member piggybacked on an accepted group post.
// Three gates: the card verifies, its fingerprint is senderFp (the envelope signer), and
// senderFp is on the roster. Then: announced-card cache for this group → friend snapshot
// (when a friend) → roster convergence (when I am the owner). Returns whether anything
// was updated. Every failure is logged and swallowed: card sync is a nicety and must
// never fail the post that carried it.
func (n *Peer) absorbGroupCard(ctx context.Context, st *a2a.GroupState, senderFp string, card *a2a.Card) bool {
	if st == nil || card == nil || senderFp == "" {
		return false
	}
	gid := st.Roster.GroupID
	if err := card.Verify(); err != nil {
		n.logf("group %s: ignoring the card %s attached (does not verify: %v)", a2a.ShortFp(gid), a2a.ShortFp(senderFp), err)
		return false
	}
	if fp, err := card.Fingerprint(); err != nil || fp != senderFp {
		n.logf("group %s: ignoring the card %s attached (fingerprint is not the sender's)", a2a.ShortFp(gid), a2a.ShortFp(senderFp))
		return false
	}
	rosterCard := st.Roster.Member(senderFp)
	if rosterCard == nil {
		return false
	}
	changed := false
	if rosterCard.Sig != card.Sig {
		stored, err := n.Groups.PutMemberCard(gid, card)
		if err != nil {
			n.logf("group %s: storing %s's announced card: %v", a2a.ShortFp(gid), a2a.ShortFp(senderFp), err)
		} else if stored {
			changed = true
			n.logf("group %s: %s announced a new card (name %q → %q)", a2a.ShortFp(gid), a2a.ShortFp(senderFp), rosterCard.Name, card.Name)
		}
	}
	if updated, err := n.RefreshFriendCard(senderFp, card); err != nil {
		n.logf("group %s: refreshing friend %s's card: %v", a2a.ShortFp(gid), a2a.ShortFp(senderFp), err)
	} else if updated {
		changed = true
	}
	if st.Roster.OwnerFp() == n.Fingerprint() && rosterCard.Sig != card.Sig {
		// Converge the roster once per card version: after this the roster carries the
		// card and the comparison above is false for every later post with the same card.
		if err := n.republishRoster(ctxOrBackground(ctx), st, nil, nil, []*a2a.Card{card}, nil); err != nil {
			n.logf("group %s: republishing the roster with %s's new card failed (retried on their next post): %v", a2a.ShortFp(gid), a2a.ShortFp(senderFp), err)
		} else {
			changed = true
			n.logf("group %s: roster republished with %s's new card (v%d)", a2a.ShortFp(gid), a2a.ShortFp(senderFp), st.Roster.Version)
			// The roster now carries it; the announced copy has nothing left to add.
			_ = n.Groups.PruneMemberCards(gid, func(fp string, _ *a2a.Card) bool { return fp != senderFp })
		}
	}
	return changed
}

// RefreshFriendCard replaces the card snapshot of friend fp with card: the card must
// verify and carry fp's key (a friend cannot hand us somebody else's card). The friend
// note follows the new nickname only while it was never hand-edited (empty, equal to the
// old nickname, or the fingerprint-prefix fallback); a note the user typed stays. Returns
// whether the snapshot changed; not a friend or the same card → false, nil.
func (n *Peer) RefreshFriendCard(fp string, card *a2a.Card) (bool, error) {
	if card == nil {
		return false, nil
	}
	fr := n.Friends.Get(fp)
	if fr == nil {
		return false, nil
	}
	if err := card.Verify(); err != nil {
		return false, fmt.Errorf("card does not verify: %w", err)
	}
	if cfp, err := card.Fingerprint(); err != nil || cfp != fp {
		return false, fmt.Errorf("card fingerprint is not %s", a2a.ShortFp(fp))
	}
	if fr.Card != nil && fr.Card.Sig == card.Sig {
		return false, nil
	}
	note := fr.Note
	if noteFollowsCard(fr) {
		note = card.Name
	}
	oldName := ""
	if fr.Card != nil {
		oldName = fr.Card.Name
	}
	if err := n.Friends.Add(card, note); err != nil {
		return false, err
	}
	if oldName != card.Name {
		n.logf("friend %s renamed %q → %q (card refreshed)", a2a.ShortFp(fp), oldName, card.Name)
	} else {
		n.logf("friend %s: card snapshot refreshed", a2a.ShortFp(fp))
	}
	return true, nil
}

// noteFollowsCard reports whether a friend's note was never hand-edited: empty, equal to
// the nickname on the card we hold, or the fingerprint-prefix fallback FriendStore.Add
// uses for a card without a nickname. Only such a note tracks a rename.
func noteFollowsCard(fr *a2a.Friend) bool {
	note := strings.TrimSpace(fr.Note)
	if note == "" {
		return true
	}
	if fr.Card != nil && note == strings.TrimSpace(fr.Card.Name) {
		return true
	}
	return len(fr.Fingerprint) >= 8 && note == fr.Fingerprint[:8]
}

// pruneAnnouncedCards reconciles the announced-card cache with a roster update the owner
// signed: a member who left is dropped, and so is a member whose roster card CHANGED in
// this update - the owner learned something newer (usually this very announcement,
// converged; possibly a fresh invite after a rename), so the roster wins from here on.
// A member whose roster card did not move keeps the announced card (the owner has not
// caught up, or never will).
func (n *Peer) pruneAnnouncedCards(gid string, prev, next *a2a.GroupRoster) {
	err := n.Groups.PruneMemberCards(gid, func(fp string, _ *a2a.Card) bool {
		nc := next.Member(fp)
		if nc == nil {
			return false
		}
		pc := prev.Member(fp)
		return pc != nil && pc.Sig == nc.Sig
	})
	if err != nil {
		n.logf("group %s: pruning announced cards: %v", a2a.ShortFp(gid), err)
	}
}

// memberCard is the freshest card known for one member: the card they announced on a
// group post themselves beats the owner-signed roster snapshot (the roster follows once
// the owner republishes; until then, or under an owner that never does, the announced
// card is the newer truth).
func memberCard(rosterCard *a2a.Card, announced map[string]*a2a.Card, fp string) *a2a.Card {
	if c := announced[fp]; c != nil {
		return c
	}
	return rosterCard
}
