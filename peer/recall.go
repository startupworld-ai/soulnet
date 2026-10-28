package peer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// Message recall (a2a.TypeRecall): the sender withdraws one of its own earlier text
// messages, pairwise or in a group. The recall names the original by id (Message.Recall)
// and travels like any other message of that channel, so it is authenticated as coming
// from its sender; a receiver honours it only when the archived original has the same
// sender. The receiver wipes the original's body and attachment from its archive and
// keeps a shell marked Recalled; the sender keeps its own text, marked Recalled too.
// How long after sending a recall is allowed is the host's policy: the peer does not
// enforce a window. Clients that predate the type log it as unknown and drop it, so the
// original stays visible there.

var (
	// ErrNoMessage: the conversation holds no message with that id.
	ErrNoMessage = errors.New("no such message")
	// ErrNotRecallable: the message is not one of my own text messages (someone else's,
	// a control or mission message), so it cannot be recalled.
	ErrNotRecallable = errors.New("only my own text messages can be recalled")
)

// RecallResult reports one recall: the original's seq in the local archive and whether
// the recall was delivered now ("sent") or parked in the outbox ("queued", pairwise only).
type RecallResult struct {
	ID     string `json:"id"`     // id of the recall message itself
	Seq    int    `json:"seq"`    // seq of the withdrawn message in my archive
	Status string `json:"status"` // sent | queued
}

// ownRecallable checks that e is a text message I sent and have not recalled yet.
func (n *Peer) ownRecallable(e *a2a.ConvEntry) error {
	if e.Dir != "out" || e.From != n.Fingerprint() || e.Type != a2a.TypeText {
		return ErrNotRecallable
	}
	return nil
}

// lookupOwn finds the archived entry id in conversation key and checks it is recallable.
// An already recalled entry is reported with done=true (recalling twice is a no-op).
func (n *Peer) lookupOwn(key, id string) (e *a2a.ConvEntry, done bool, err error) {
	_, e, err = n.Convs.Update(key, id, func(*a2a.ConvEntry) (bool, error) { return false, nil })
	if errors.Is(err, a2a.ErrNoEntry) {
		return nil, false, ErrNoMessage
	}
	if err != nil {
		return nil, false, err
	}
	if err := n.ownRecallable(e); err != nil {
		return nil, false, err
	}
	return e, e.Recalled, nil
}

// markOwnRecalled flags my archived copy (the text is kept) and returns its seq.
func (n *Peer) markOwnRecalled(key, id string) (int, error) {
	seq, _, err := n.Convs.Update(key, id, func(e *a2a.ConvEntry) (bool, error) {
		if e.Recalled {
			return false, nil
		}
		e.Recalled = true
		return true, nil
	})
	return seq, err
}

// Recall withdraws one of my text messages to friend fp (msgID = its id). The recall is
// delivered now or queued in the outbox like any message (queued: Status "queued", nil
// error), and my archived copy is marked Recalled with its text kept. Errors:
// ErrNotFriend, ErrNoMessage, ErrNotRecallable, or a delivery failure that could not
// even be queued. Recalling an already recalled message succeeds without sending.
func (n *Peer) Recall(ctx context.Context, fp, msgID string) (*RecallResult, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	fr := n.Friends.Get(fp)
	if fr == nil || fr.Card == nil {
		return nil, ErrNotFriend
	}
	_, done, err := n.lookupOwn(fp, msgID)
	if err != nil {
		return nil, err
	}
	res := &RecallResult{Status: "sent"}
	if !done {
		msg := &a2a.Message{Type: a2a.TypeRecall, Recall: msgID}
		sr, err := n.SendMessage(ctx, fr.Card, msg, MessageOptions{})
		if err != nil && !errors.Is(err, ErrQueued) {
			return nil, err
		}
		if errors.Is(err, ErrQueued) {
			res.Status = "queued"
		}
		if sr != nil {
			res.ID = sr.ID
		}
	}
	if res.Seq, err = n.markOwnRecalled(fp, msgID); err != nil {
		return nil, err
	}
	return res, nil
}

// GroupRecall withdraws one of my posts in group gid. The recall is fanned out like a
// post (group fan-out has no outbox: a delivery failure is returned and my archive is
// left untouched, so the caller can retry). The speak switches do not apply: taking
// words back is always allowed to a member. Errors: ErrNoGroup, ErrGroupLeft,
// ErrNoMessage, ErrNotRecallable, or the delivery error.
func (n *Peer) GroupRecall(ctx context.Context, gid, msgID string) (*RecallResult, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return nil, ErrNoGroup
	}
	if n.GroupLeft(gid) {
		return nil, ErrGroupLeft
	}
	key := a2a.GroupConvKey(gid)
	_, done, err := n.lookupOwn(key, msgID)
	if err != nil {
		return nil, err
	}
	res := &RecallResult{Status: "sent"}
	if !done {
		msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), GID: gid, TS: time.Now(),
			Type: a2a.TypeRecall, Recall: msgID}
		if err := n.fanOutGroup(ctxOrBackground(ctx), st, msg); err != nil {
			return nil, err
		}
		res.ID = msg.ID
		n.logf("<<< group recall gid=%s of %s", a2a.ShortFp(gid), msgID)
	}
	if res.Seq, err = n.markOwnRecalled(key, msgID); err != nil {
		return nil, err
	}
	return res, nil
}

// applyRecall is the receiving side for both channels: key is the conversation (friend
// fingerprint, or GroupConvKey(gid)), sender the authenticated sender of the recall,
// dirPeer the attachments directory key. The original must be a text message from the
// same sender; its body and attachment are wiped and it is marked Recalled, then
// message.recalled is emitted. An original that has not arrived yet (in flight, still
// being chunked) is waited for within the retry budget; a recall of someone else's
// message is rejected for good.
func (n *Peer) applyRecall(key, dirPeer, sender, gid string, msg *a2a.Message) error {
	if msg.Recall == "" {
		return permanent(fmt.Errorf("recall without a message id"))
	}
	var files []string
	seq, e, err := n.Convs.Update(key, msg.Recall, func(e *a2a.ConvEntry) (bool, error) {
		if e.From != sender || e.Dir != "in" {
			return false, permanent(fmt.Errorf("recall of %s from %s: the message is not theirs", msg.Recall, a2a.ShortFp(sender)))
		}
		if e.Type != a2a.TypeText {
			return false, permanent(fmt.Errorf("recall of %s: a %s message cannot be recalled", msg.Recall, e.Type))
		}
		if e.Recalled {
			return false, nil
		}
		if e.ArtifactName != "" && a2a.ValidArtifactName(e.ArtifactName) {
			files = append(files, n.ArtifactPath(dirPeer, e.ID, e.ArtifactName))
			if e.ArtifactID != "" {
				files = append(files, n.ArtifactPath(dirPeer, e.ArtifactID, e.ArtifactName))
			}
		}
		e.Body = ""
		clearArtifact(&e.Message)
		e.QuoteMission = ""
		e.Recalled = true
		return true, nil
	})
	if errors.Is(err, a2a.ErrNoEntry) {
		return n.transientOrDrop("recall-"+key+msg.Recall, fmt.Errorf("recall of %s: the message has not arrived (yet)", msg.Recall))
	}
	if err != nil {
		return err
	}
	for _, f := range files {
		_ = os.Remove(f) // the words are withdrawn, so is the file; best effort
	}
	n.logf("message %s recalled by %s", msg.Recall, a2a.ShortFp(sender))
	n.emit(Event{Kind: EventMessageRecalled, Peer: sender, GID: gid, TS: time.Now(), Message: msg, Seq: seq, Recalled: e.ID})
	return nil
}

// handleRecall (pairwise): only friends' recalls count (their messages are the only
// ones archived under their fingerprint anyway).
func (n *Peer) handleRecall(msg *a2a.Message) error {
	if !n.Friends.IsFriend(msg.From) {
		n.logf("dropping recall from non-friend %s", a2a.ShortFp(msg.From))
		return nil
	}
	return n.applyRecall(msg.From, msg.From, msg.From, "", msg)
}
