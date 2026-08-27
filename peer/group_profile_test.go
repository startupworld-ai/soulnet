// Profile-era group flows over a real relay: governance roundtrip and AllowSpeak
// enforcement on both ends, pins, setProfile, the three join policies, and the admin
// invite/kick paths (spec §14.7).
package peer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestGroupProfileRoundtripAndAllowSpeak: creating a group signs the profile into the
// roster (invitees hold it), and the speak switches bite on BOTH ends — the sender
// refuses locally, and a receiver drops a violating message crafted by a tampered
// sender with the real keys.
func TestGroupProfileRoundtripAndAllowSpeak(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()

	prof := a2a.DefaultGroupProfile()
	prof.SpeakHumans = false // agents-only group
	view, err := alice.GroupCreate(ctx, "agents only", []string{bob.Fingerprint()}, prof)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	if view.Profile == nil || view.Profile.SpeakHumans || view.MyRole != "owner" {
		t.Fatalf("owner view: %+v", view)
	}
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	st := bob.Groups.Get(gid)
	if st == nil || st.Roster.Profile == nil || st.Roster.Profile.SpeakHumans || st.Roster.Profile.Template != "standard" {
		t.Fatalf("bob's roster profile did not round-trip: %+v", st)
	}
	if bv, err := bob.GroupInfo(gid); err != nil || bv.MyRole != "member" || bv.Profile == nil {
		t.Fatalf("bob view: %+v %v", bv, err)
	}
	if sums := bob.GroupList(); len(sums) != 1 || sums[0].Profile == nil || sums[0].Profile.SpeakHumans {
		t.Fatalf("bob list rows carry no profile: %+v", sums)
	}

	// Local enforcement: a human post is refused without sending — even the owner's.
	if _, err := alice.GroupSend(ctx, gid, "as human", GroupSendOptions{}); err == nil {
		t.Fatalf("human send accepted in an agents-only group")
	}
	// An agent post goes through and keeps its provenance.
	if _, err := alice.GroupSend(ctx, gid, "as alter", GroupSendOptions{By: a2a.ByAlter, Auto: true}); err != nil {
		t.Fatalf("alter send: %v", err)
	}
	ev := bob.await(t, "bob gets the alter post", func(e Event) bool { return e.Kind == EventGroupMessage && e.GID == gid })
	if ev.Message.By != a2a.ByAlter || !ev.Message.Auto {
		t.Fatalf("provenance lost: %+v", ev.Message)
	}

	// Receiver-side enforcement: bob's node is "tampered" to skip the local check and
	// fans a human (by="") message with its real keys; alice must drop it. The chain
	// advance happens under bob's key mutex, exactly like a real send.
	msg := &a2a.Message{ID: a2a.NewMessageID(bob.Fingerprint()), From: bob.Fingerprint(), GID: gid,
		TS: time.Now(), Type: a2a.TypeText, Body: "human sneaking in"}
	bob.gkMu.Lock()
	keys := bob.Groups.Keys(gid)
	if keys.Mine == nil {
		bob.gkMu.Unlock()
		t.Fatalf("bob has no sender key")
	}
	cipher, err := a2a.GroupSeal(keys.Mine, msg)
	if err != nil {
		bob.gkMu.Unlock()
		t.Fatalf("GroupSeal: %v", err)
	}
	if err := bob.Groups.PutKeys(gid, keys); err != nil { // keep bob's chain consistent
		bob.gkMu.Unlock()
		t.Fatalf("PutKeys: %v", err)
	}
	bob.gkMu.Unlock()
	env, err := a2a.SealGroupEnvelope(bob.Identity(), gid, cipher)
	if err != nil {
		t.Fatalf("SealGroupEnvelope: %v", err)
	}
	if err := a2a.NewProxyClient(relayURL, bob.Identity()).DeliverGroup(ctx, env); err != nil {
		t.Fatalf("DeliverGroup: %v", err)
	}
	// A compliant message sent AFTER the violation must arrive — the violation must not.
	if _, err := bob.GroupSend(ctx, gid, "legit alter post", GroupSendOptions{By: a2a.ByAlter}); err != nil {
		t.Fatalf("bob alter send: %v", err)
	}
	alice.await(t, "alice gets the legit post", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "legit alter post"
	})
	for _, e := range alice.GroupConversation(gid, 0, 0) {
		if strings.Contains(e.Body, "human sneaking in") {
			t.Fatalf("violating message was archived")
		}
	}
}

// TestGroupPins: the owner pins and members converge (with group.updated raised);
// non-admins are refused locally; unpin converges too; pins never touch the chat stream.
func TestGroupPins(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()

	view, err := alice.GroupCreate(ctx, "pin club", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	if _, err := bob.GroupPin(ctx, gid, "not allowed"); err == nil {
		t.Fatalf("non-admin pin accepted")
	}
	pin, err := alice.GroupPin(ctx, gid, "welcome to the club")
	if err != nil {
		t.Fatalf("GroupPin: %v", err)
	}
	if got := alice.Groups.Pins(gid); len(got) != 1 || got[0].Body != "welcome to the club" {
		t.Fatalf("owner pins: %+v", got)
	}
	bob.await(t, "bob sees the pin", func(e Event) bool {
		return e.Kind == EventGroupUpdated && e.GID == gid && len(bob.Groups.Pins(gid)) == 1
	})
	bv, err := bob.GroupInfo(gid)
	if err != nil || len(bv.Pins) != 1 || bv.Pins[0].ID != pin.ID || bv.Pins[0].From != alice.Fingerprint() {
		t.Fatalf("bob view pins: %+v %v", bv, err)
	}
	if got := len(bob.GroupConversation(gid, 0, 0)); got != 0 {
		t.Fatalf("pin leaked into the chat stream: %d entries", got)
	}
	if err := alice.GroupUnpin(ctx, gid, pin.ID); err != nil {
		t.Fatalf("GroupUnpin: %v", err)
	}
	if got := alice.Groups.Pins(gid); len(got) != 0 {
		t.Fatalf("owner pins after unpin: %+v", got)
	}
	waitUntil(t, "bob's pin is gone", func() bool { return len(bob.Groups.Pins(gid)) == 0 })
}

// TestGroupSetProfile: the owner republishes the profile (roster v2) and members see it;
// the flipped switch is enforced immediately; non-owners are refused.
func TestGroupSetProfile(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()

	view, err := alice.GroupCreate(ctx, "flip club", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	prof := a2a.DefaultGroupProfile()
	prof.SpeakAgents = false
	if err := bob.GroupSetProfile(ctx, gid, prof); err == nil {
		t.Fatalf("non-owner setProfile accepted")
	}
	if err := alice.GroupSetProfile(ctx, gid, prof); err != nil {
		t.Fatalf("GroupSetProfile: %v", err)
	}
	waitUntil(t, "bob sees the roster v2 profile", func() bool {
		st := bob.Groups.Get(gid)
		return st != nil && st.Roster.Version == 2 && st.Roster.Profile != nil && !st.Roster.Profile.SpeakAgents
	})
	if _, err := alice.GroupSend(ctx, gid, "beep", GroupSendOptions{By: a2a.ByAlter}); err == nil {
		t.Fatalf("alter send accepted after speak_agents=false")
	}
}

// TestGroupJoinFlows drives the three join policies with STRANGERS (no friendship with
// the owner): apply pends + approve/reject, open adds mechanically, invite drops.
func TestGroupJoinFlows(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()

	// ——— apply mode ———
	prof := a2a.DefaultGroupProfile()
	prof.Join = a2a.JoinApply
	prof.Public = true
	view, err := alice.GroupCreate(ctx, "apply club", []string{bob.Fingerprint()}, prof)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	uri := a2a.EncodeGroupURI(gid, relayURL, "apply club")

	dave := newTestNode(t, relayURL, "dave") // stranger: not a friend of alice
	gotGid, err := dave.GroupApply(ctx, uri, "let me in", nil)
	if err != nil || gotGid != gid {
		t.Fatalf("GroupApply: %q %v", gotGid, err)
	}
	appEv := alice.await(t, "application lands", func(e Event) bool { return e.Kind == EventGroupApplication && e.GID == gid })
	if appEv.Peer != dave.Fingerprint() || appEv.Message == nil || appEv.Message.Body != "let me in" {
		t.Fatalf("application event: %+v", appEv)
	}
	apps, err := alice.GroupApplications(gid)
	if err != nil || len(apps) != 1 || apps[0].Fp != dave.Fingerprint() || apps[0].Name != "dave" || apps[0].Note != "let me in" {
		t.Fatalf("applications: %+v %v", apps, err)
	}
	if av, err := alice.GroupInfo(gid); err != nil || len(av.Applications) != 1 {
		t.Fatalf("owner view misses the application: %+v %v", av, err)
	}
	if err := alice.GroupApprove(ctx, gid, dave.Fingerprint()); err != nil {
		t.Fatalf("GroupApprove: %v", err)
	}
	waitUntil(t, "dave holds the group", func() bool { return dave.Groups.Get(gid) != nil })
	if apps, _ := alice.GroupApplications(gid); len(apps) != 0 {
		t.Fatalf("application not consumed: %+v", apps)
	}
	if _, err := dave.GroupSend(ctx, gid, "thanks for having me", GroupSendOptions{}); err != nil {
		t.Fatalf("dave GroupSend: %v", err)
	}
	alice.await(t, "alice hears dave", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "thanks for having me"
	})

	// Reject path.
	erin := newTestNode(t, relayURL, "erin")
	if _, err := erin.GroupApply(ctx, uri, "me too", nil); err != nil {
		t.Fatalf("erin GroupApply: %v", err)
	}
	alice.await(t, "erin's application lands", func(e Event) bool {
		return e.Kind == EventGroupApplication && e.GID == gid && e.Peer == erin.Fingerprint()
	})
	if err := alice.GroupRejectApplication(gid, erin.Fingerprint()); err != nil {
		t.Fatalf("GroupRejectApplication: %v", err)
	}
	if apps, _ := alice.GroupApplications(gid); len(apps) != 0 {
		t.Fatalf("rejected application lingers: %+v", apps)
	}

	// ——— open mode: a stranger is added mechanically ———
	oprof := a2a.DefaultGroupProfile()
	oprof.Join = a2a.JoinOpen
	oprof.Public = true
	oview, err := alice.GroupCreate(ctx, "open club", []string{bob.Fingerprint()}, oprof)
	if err != nil {
		t.Fatalf("GroupCreate open: %v", err)
	}
	ouri := a2a.EncodeGroupURI(oview.GID, relayURL, "open club")
	frank := newTestNode(t, relayURL, "frank")
	if _, err := frank.GroupApply(ctx, ouri, "", nil); err != nil {
		t.Fatalf("frank GroupApply: %v", err)
	}
	waitUntil(t, "frank auto-joins the open group", func() bool { return frank.Groups.Get(oview.GID) != nil })

	// ——— invite mode: applications are dropped ———
	iprof := a2a.DefaultGroupProfile()
	iprof.Public = true // discoverable, but join stays invite-only
	iview, err := alice.GroupCreate(ctx, "invite club", []string{bob.Fingerprint()}, iprof)
	if err != nil {
		t.Fatalf("GroupCreate invite: %v", err)
	}
	iuri := a2a.EncodeGroupURI(iview.GID, relayURL, "invite club")
	grace := newTestNode(t, relayURL, "grace")
	if _, err := grace.GroupApply(ctx, iuri, "please", nil); err != nil {
		t.Fatalf("grace GroupApply: %v", err)
	}
	time.Sleep(1 * time.Second)
	if grace.Groups.Get(iview.GID) != nil {
		t.Fatalf("invite-only group auto-joined a stranger")
	}
	if apps, _ := alice.GroupApplications(iview.GID); len(apps) != 0 {
		t.Fatalf("invite-only group pended an application: %+v", apps)
	}
}

// TestGroupAdminInviteAndKick: the owner promotes an admin via setProfile; the admin
// invites its own friend (a stranger to the owner) through the group_admin path — the
// owner republishes, the admin forwards the invite, the friend joins and chats — and
// later kicks them through the same path.
func TestGroupAdminInviteAndKick(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, bob, carol) // carol is bob's friend, a STRANGER to alice
	ctx := context.Background()

	view, err := alice.GroupCreate(ctx, "admin club", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	// Before promotion the admin path is refused.
	if err := bob.GroupInvite(ctx, gid, carol.Fingerprint()); err == nil {
		t.Fatalf("non-admin invite accepted")
	}

	prof := a2a.DefaultGroupProfile()
	prof.Admins = []string{bob.Fingerprint()}
	if err := alice.GroupSetProfile(ctx, gid, prof); err != nil {
		t.Fatalf("GroupSetProfile: %v", err)
	}
	waitUntil(t, "bob sees himself as admin", func() bool {
		st := bob.Groups.Get(gid)
		return st != nil && st.Roster.Profile.IsAdmin(bob.Fingerprint())
	})
	if bv, err := bob.GroupInfo(gid); err != nil || bv.MyRole != "admin" {
		t.Fatalf("bob's role: %+v %v", bv, err)
	}

	// Admin invite: bob asks the owner; the owner republishes; bob forwards the invite.
	if err := bob.GroupInvite(ctx, gid, carol.Fingerprint()); err != nil {
		t.Fatalf("admin invite: %v", err)
	}
	waitUntil(t, "carol joins", func() bool { return carol.Groups.Get(gid) != nil })
	waitUntil(t, "alice's roster includes carol", func() bool {
		st := alice.Groups.Get(gid)
		return st != nil && st.Roster.Member(carol.Fingerprint()) != nil
	})
	if _, err := carol.GroupSend(ctx, gid, "hi from carol", GroupSendOptions{}); err != nil {
		t.Fatalf("carol GroupSend: %v", err)
	}
	alice.await(t, "alice hears carol", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "hi from carol"
	})

	// Admin kick: bob asks the owner to remove carol.
	if err := bob.GroupKick(ctx, gid, carol.Fingerprint()); err != nil {
		t.Fatalf("admin kick: %v", err)
	}
	waitUntil(t, "carol is out", func() bool { return carol.Groups.Get(gid) == nil })
	waitUntil(t, "alice's roster drops carol", func() bool {
		st := alice.Groups.Get(gid)
		return st != nil && st.Roster.Member(carol.Fingerprint()) == nil
	})
}

// TestGroupPaidJoin: a stranger pays (attaches a JoinPayment proof to the
// group_join) and the owner's node pends the application with the payment
// carried through, ready for the host to verify.
func TestGroupPaidJoin(t *testing.T) {
	relayURL := startRelay(t)
	ctx := context.Background()
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)

	prof := a2a.DefaultGroupProfile()
	prof.Join = a2a.JoinPaid
	prof.JoinPrice = "1.00"
	prof.JoinAddr = "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29"
	prof.Public = true
	view, err := alice.GroupCreate(ctx, "paid club", []string{bob.Fingerprint()}, prof)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	uri := a2a.EncodeGroupURI(gid, relayURL, "paid club")

	dave := newTestNode(t, relayURL, "dave")
	payment := &a2a.JoinPayment{
		TxHash: "0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7",
		Amount: "1.00",
		To:     "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29",
	}
	if _, err := dave.GroupApply(ctx, uri, "paid, here is the tx", payment); err != nil {
		t.Fatalf("paid GroupApply: %v", err)
	}
	appEv := alice.await(t, "paid application lands", func(e Event) bool { return e.Kind == EventGroupApplication && e.GID == gid })
	if appEv.Message == nil || appEv.Message.Payment == nil || appEv.Message.Payment.TxHash != payment.TxHash {
		t.Fatalf("application event lost the payment: %+v", appEv)
	}
	apps, err := alice.GroupApplications(gid)
	if err != nil || len(apps) != 1 || apps[0].Payment == nil || apps[0].Payment.Amount != "1.00" || apps[0].Payment.To != payment.To {
		t.Fatalf("application view missing payment: %+v %v", apps, err)
	}
	// owner approves after (host-side) verification
	if err := alice.GroupApprove(ctx, gid, dave.Fingerprint()); err != nil {
		t.Fatalf("GroupApprove: %v", err)
	}
	waitUntil(t, "dave holds the group", func() bool { return dave.Groups.Get(gid) != nil })
}

// TestGroupPaidJoinReplayRefused: the consumed-tx ledger is the replay guard —
// after one applicant's payment tx admits them, a SECOND applicant submitting
// the very same tx_hash as their own proof must be refused at approve time
// (the tx is already consumed).
func TestGroupPaidJoinReplayRefused(t *testing.T) {
	relayURL := startRelay(t)
	ctx := context.Background()
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)

	prof := a2a.DefaultGroupProfile()
	prof.Join = a2a.JoinPaid
	prof.JoinPrice = "1.00"
	prof.JoinAddr = "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29"
	prof.Public = true
	view, err := alice.GroupCreate(ctx, "paid club", []string{bob.Fingerprint()}, prof)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	uri := a2a.EncodeGroupURI(gid, relayURL, "paid club")

	dave := newTestNode(t, relayURL, "dave")
	erin := newTestNode(t, relayURL, "erin")
	// Both applicants attach the SAME on-chain transfer as their proof (the
	// replay attack: browsing the chain and claiming someone else's payment).
	payment := &a2a.JoinPayment{
		TxHash: "0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7",
		Amount: "1.00",
		To:     "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29",
	}
	if _, err := dave.GroupApply(ctx, uri, "paid", payment); err != nil {
		t.Fatalf("dave GroupApply: %v", err)
	}
	if _, err := erin.GroupApply(ctx, uri, "paid too", payment); err != nil {
		t.Fatalf("erin GroupApply: %v", err)
	}
	alice.await(t, "dave application lands", func(e Event) bool {
		return e.Kind == EventGroupApplication && e.GID == gid && e.Peer == dave.Fingerprint()
	})
	alice.await(t, "erin application lands", func(e Event) bool {
		return e.Kind == EventGroupApplication && e.GID == gid && e.Peer == erin.Fingerprint()
	})

	// First approve consumes the tx and admits dave.
	if err := alice.GroupApprove(ctx, gid, dave.Fingerprint()); err != nil {
		t.Fatalf("first GroupApprove: %v", err)
	}
	waitUntil(t, "dave holds the group", func() bool { return dave.Groups.Get(gid) != nil })

	// The second approve reuses the same tx_hash — it must be refused.
	err = alice.GroupApprove(ctx, gid, erin.Fingerprint())
	if err == nil {
		t.Fatal("replayed payment tx must be refused at approve")
	}
	if !errors.Is(err, ErrPaidProofUsed) {
		t.Fatalf("expected ErrPaidProofUsed, got %v", err)
	}
	// Erin is NOT admitted, and the application stays pending for a real proof.
	if st := erin.Groups.Get(gid); st != nil {
		t.Fatal("erin must not be admitted with a replayed tx")
	}
	apps, err := alice.GroupApplications(gid)
	if err != nil || len(apps) != 1 || apps[0].Fp != erin.Fingerprint() {
		t.Fatalf("erin's application should still be pending: %+v %v", apps, err)
	}
}
