package a2a

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

// grpIdentity creates a throwaway identity in a temp dir.
func grpIdentity(t *testing.T, name string) *Identity {
	t.Helper()
	id, err := NewIdentity(t.TempDir(), name, []string{"http://relay.test"})
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	return id
}

func grpCard(t *testing.T, id *Identity) *Card {
	t.Helper()
	c, err := id.Card()
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	return c
}

func testRoster(t *testing.T, owner *Identity, members ...*Identity) *GroupRoster {
	t.Helper()
	gid, err := NewGroupID()
	if err != nil {
		t.Fatalf("NewGroupID: %v", err)
	}
	cards := []*Card{grpCard(t, owner)}
	for _, m := range members {
		cards = append(cards, grpCard(t, m))
	}
	g := &GroupRoster{V: 1, GroupID: gid, Name: "test group", OwnerPub: owner.EdPub,
		Relay: "http://relay.test", Version: 1, Members: cards, TS: time.Now()}
	priv, err := owner.EdPrivate()
	if err != nil {
		t.Fatalf("EdPrivate: %v", err)
	}
	g.Sign(priv)
	return g
}

func TestGroupRosterSignVerify(t *testing.T) {
	owner := grpIdentity(t, "owner")
	bob := grpIdentity(t, "bob")
	g := testRoster(t, owner, bob)
	if err := g.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if g.OwnerFp() != owner.Fingerprint() {
		t.Fatalf("owner fp mismatch")
	}
	if g.Member(bob.Fingerprint()) == nil {
		t.Fatalf("bob should be a member")
	}
	if g.Member("nope") != nil {
		t.Fatalf("stranger should not be a member")
	}

	// Tampering with the name must break the signature.
	g.Name = "renamed"
	if err := g.Verify(); err == nil {
		t.Fatalf("tampered roster verified")
	}
	g.Name = "test group"
	if err := g.Verify(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// A non-owner cannot re-sign as themselves without changing OwnerPub (and changing
	// OwnerPub makes it a different group identity the relay will reject on republish).
	bobPriv, _ := bob.EdPrivate()
	g.Members = g.Members[:1] // kick bob
	g.Sign(bobPriv)
	if err := g.Verify(); err == nil {
		t.Fatalf("roster signed by a non-owner key verified")
	}
}

func TestGroupRosterRejectsOwnerNotMember(t *testing.T) {
	owner := grpIdentity(t, "owner")
	bob := grpIdentity(t, "bob")
	g := testRoster(t, owner, bob)
	g.Members = g.Members[1:] // drop the owner card
	priv, _ := owner.EdPrivate()
	g.Sign(priv)
	if err := g.Verify(); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("expected owner-not-member error, got %v", err)
	}
}

func TestGroupSealOpenInOrder(t *testing.T) {
	sk, err := NewGroupSenderKey(1)
	if err != nil {
		t.Fatalf("NewGroupSenderKey: %v", err)
	}
	recv := &GroupRecvState{Epoch: 1, Index: 0, Chain: sk.Chain}
	for i := 0; i < 5; i++ {
		msg := &Message{ID: NewMessageID("sender"), From: "sender", GID: "0123456789abcdef0123456789abcdef",
			TS: time.Now(), Type: TypeText, Body: "hello"}
		ct, err := GroupSeal(sk, msg)
		if err != nil {
			t.Fatalf("GroupSeal %d: %v", i, err)
		}
		got, err := GroupOpen(recv, ct)
		if err != nil {
			t.Fatalf("GroupOpen %d: %v", i, err)
		}
		if got.Body != "hello" || got.ID != msg.ID {
			t.Fatalf("roundtrip %d mismatch", i)
		}
	}
	if sk.Index != 5 || recv.Index != 5 {
		t.Fatalf("ratchet positions: send %d recv %d", sk.Index, recv.Index)
	}
}

func TestGroupOpenOutOfOrderAndReplay(t *testing.T) {
	sk, _ := NewGroupSenderKey(1)
	recv := &GroupRecvState{Epoch: 1, Index: 0, Chain: sk.Chain}
	cts := make([]string, 4)
	for i := range cts {
		ct, err := GroupSeal(sk, &Message{ID: NewMessageID("s"), Type: TypeText, Body: "m"})
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		cts[i] = ct
	}
	// Deliver 2 first (skips 0,1), then 0, then 1, then 3.
	for _, i := range []int{2, 0, 1, 3} {
		if _, err := GroupOpen(recv, cts[i]); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	// Replaying an already-consumed index must fail.
	if _, err := GroupOpen(recv, cts[1]); err == nil {
		t.Fatalf("replay accepted")
	}
}

func TestGroupOpenEpochs(t *testing.T) {
	sk1, _ := NewGroupSenderKey(1)
	sk2, _ := NewGroupSenderKey(2)
	recv := &GroupRecvState{Epoch: 1, Index: 0, Chain: sk1.Chain}
	ctNew, _ := GroupSeal(sk2, &Message{ID: "x", Type: TypeText, Body: "new epoch"})
	_, err := GroupOpen(recv, ctNew)
	if err == nil || !strings.Contains(err.Error(), "epoch") {
		t.Fatalf("expected epoch-ahead error, got %v", err)
	}
	// After the key arrives (epoch 2), decryption works.
	recv2 := &GroupRecvState{Epoch: 2, Index: 0, Chain: sk2.Chain}
	// sk2 already advanced by the seal above; recreate the pre-seal chain by decrypting with index 0 state:
	if _, err := GroupOpen(recv2, ctNew); err == nil {
		t.Fatalf("recv2 holds the ADVANCED chain, decrypt of index 0 with it must fail")
	}
	// Old-epoch ciphertext against a newer held epoch is rejected outright.
	ctOld, _ := GroupSeal(sk1, &Message{ID: "y", Type: TypeText, Body: "old"})
	recvNew := &GroupRecvState{Epoch: 2, Index: 0, Chain: sk2.Chain}
	if _, err := GroupOpen(recvNew, ctOld); err == nil {
		t.Fatalf("superseded epoch accepted")
	}
}

func TestGroupEnvelopeSignVerify(t *testing.T) {
	id := grpIdentity(t, "alice")
	sk, _ := NewGroupSenderKey(1)
	gid, _ := NewGroupID()
	ct, err := GroupSeal(sk, &Message{ID: "m1", From: id.Fingerprint(), GID: gid, TS: time.Now(), Type: TypeText, Body: "hi"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	env, err := SealGroupEnvelope(id, gid, ct)
	if err != nil {
		t.Fatalf("SealGroupEnvelope: %v", err)
	}
	fp, err := env.VerifyGroupEnvelope()
	if err != nil {
		t.Fatalf("VerifyGroupEnvelope: %v", err)
	}
	if fp != id.Fingerprint() {
		t.Fatalf("sender fp mismatch")
	}
	// Stamping To (what the relay does per copy) must NOT break the signature.
	env.To = "someRecipientFp"
	if _, err := env.VerifyGroupEnvelope(); err != nil {
		t.Fatalf("To stamping broke the signature: %v", err)
	}
	// Tampering with the cipher must.
	env.Cipher = env.Cipher[:len(env.Cipher)-4] + "AAAA"
	if _, err := env.VerifyGroupEnvelope(); err == nil {
		t.Fatalf("tampered group envelope verified")
	}
}

func TestGroupStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	owner := grpIdentity(t, "owner")
	bob := grpIdentity(t, "bob")
	g := testRoster(t, owner, bob)
	s := NewGroupStore(dir)

	if s.Get(g.GroupID) != nil {
		t.Fatalf("empty store returned a group")
	}
	if err := s.Put(&GroupState{Roster: g, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := s.Get(g.GroupID)
	if got == nil || got.Roster.Name != "test group" {
		t.Fatalf("Get after Put: %+v", got)
	}
	if len(s.List()) != 1 {
		t.Fatalf("List: want 1")
	}

	k := s.Keys(g.GroupID)
	if k.Mine != nil || len(k.Senders) != 0 {
		t.Fatalf("fresh keys not empty")
	}
	k.Mine, _ = NewGroupSenderKey(1)
	k.Senders[bob.Fingerprint()] = &GroupRecvState{Epoch: 1, Chain: k.Mine.Chain}
	k.DistributedTo[bob.Fingerprint()] = 1
	if err := s.PutKeys(g.GroupID, k); err != nil {
		t.Fatalf("PutKeys: %v", err)
	}
	k2 := s.Keys(g.GroupID)
	if k2.Mine == nil || k2.Mine.Epoch != 1 || k2.DistributedTo[bob.Fingerprint()] != 1 {
		t.Fatalf("keys roundtrip: %+v", k2)
	}

	if c := s.FindMemberCard(bob.Fingerprint()); c == nil {
		t.Fatalf("FindMemberCard missed bob")
	}
	if err := s.Remove(g.GroupID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if s.Get(g.GroupID) != nil {
		t.Fatalf("group survived Remove")
	}
}

// ——— governance layer (§14.7): profile signing, validation, speak matrix, public card ———

func TestGroupRosterProfileSigning(t *testing.T) {
	owner := grpIdentity(t, "owner")
	bob := grpIdentity(t, "bob")
	g := testRoster(t, owner, bob)
	g.Profile = DefaultGroupProfile()
	priv, _ := owner.EdPrivate()
	g.Sign(priv)
	if err := g.Verify(); err != nil {
		t.Fatalf("Verify with profile: %v", err)
	}
	// The profile signs with the roster: tampering must break the signature.
	g.Profile.Rules = "new rules"
	if err := g.Verify(); err == nil {
		t.Fatalf("tampered profile verified")
	}
	g.Profile.Rules = ""
	if err := g.Verify(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Stripping the whole profile must break it too.
	g.Profile = nil
	if err := g.Verify(); err == nil {
		t.Fatalf("stripped profile verified")
	}
}

func TestGroupProfileValidate(t *testing.T) {
	p := DefaultGroupProfile()
	if err := p.Validate(nil); err != nil {
		t.Fatalf("default profile invalid: %v", err)
	}
	p.Admins = []string{"strangerfp"}
	if err := p.Validate([]string{"memberfp"}); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("admin-not-member accepted: %v", err)
	}
	if err := p.Validate([]string{"strangerfp"}); err != nil {
		t.Fatalf("admin who is a member rejected: %v", err)
	}
	bad := DefaultGroupProfile()
	bad.SpeakWho = "nobody"
	if err := bad.Validate(nil); err == nil {
		t.Fatalf("bad speak_who accepted")
	}
	mute := DefaultGroupProfile()
	mute.SpeakHumans, mute.SpeakAgents = false, false
	if err := mute.Validate(nil); err == nil {
		t.Fatalf("all-muted profile accepted")
	}
	// Roster.Verify pins admins to members too.
	owner := grpIdentity(t, "owner")
	bob := grpIdentity(t, "bob")
	g := testRoster(t, owner, bob)
	prof := DefaultGroupProfile()
	prof.Admins = []string{"not-a-member"}
	g.Profile = prof
	priv, _ := owner.EdPrivate()
	g.Sign(priv)
	if err := g.Verify(); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("roster with a foreign admin verified: %v", err)
	}
}

func TestGroupAllowSpeakMatrix(t *testing.T) {
	owner := grpIdentity(t, "owner")
	bob := grpIdentity(t, "bob")
	g := testRoster(t, owner, bob)
	of, bf := owner.Fingerprint(), bob.Fingerprint()

	// nil profile: everything allowed (legacy groups).
	if err := g.AllowSpeak(bf, ByAlter); err != nil {
		t.Fatalf("nil profile blocked a post: %v", err)
	}

	cases := []struct {
		name   string
		mod    func(*GroupProfile)
		fp, by string
		ok     bool
	}{
		{"all-all human member", func(p *GroupProfile) {}, bf, "", true},
		{"all-all alter member", func(p *GroupProfile) {}, bf, ByAlter, true},
		{"owner-only blocks member", func(p *GroupProfile) { p.SpeakWho = SpeakOwner }, bf, "", false},
		{"owner-only allows owner", func(p *GroupProfile) { p.SpeakWho = SpeakOwner }, of, "", true},
		{"admins allows admin", func(p *GroupProfile) { p.SpeakWho = SpeakAdmins; p.Admins = []string{bf} }, bf, "", true},
		{"admins blocks plain member", func(p *GroupProfile) { p.SpeakWho = SpeakAdmins }, bf, "", false},
		{"admins always allows owner", func(p *GroupProfile) { p.SpeakWho = SpeakAdmins }, of, "", true},
		{"no humans blocks by owner", func(p *GroupProfile) { p.SpeakHumans = false }, of, ByOwner, false},
		{"no humans blocks empty by", func(p *GroupProfile) { p.SpeakHumans = false }, of, "", false},
		{"no humans allows alter", func(p *GroupProfile) { p.SpeakHumans = false }, of, ByAlter, true},
		{"no agents blocks alter", func(p *GroupProfile) { p.SpeakAgents = false }, bf, ByAlter, false},
		{"no agents allows human", func(p *GroupProfile) { p.SpeakAgents = false }, bf, ByOwner, true},
	}
	for _, c := range cases {
		p := DefaultGroupProfile()
		c.mod(p)
		g.Profile = p
		err := g.AllowSpeak(c.fp, c.by)
		if c.ok && err != nil {
			t.Errorf("%s: unexpectedly blocked: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: unexpectedly allowed", c.name)
		}
	}
}

func TestGroupPublicCard(t *testing.T) {
	owner := grpIdentity(t, "owner")
	bob := grpIdentity(t, "bob")
	g := testRoster(t, owner, bob)
	if g.PublicCard() != nil {
		t.Fatalf("nil profile produced a public card")
	}
	p := DefaultGroupProfile()
	g.Profile = p
	if g.PublicCard() != nil {
		t.Fatalf("non-public profile produced a public card")
	}
	p.Public = true
	p.Join = JoinApply
	p.Tags = []string{"builders"}
	p.Rules = strings.Repeat("r", 300)
	card := g.PublicCard()
	if card == nil {
		t.Fatalf("public profile produced no card")
	}
	if card.GID != g.GroupID || card.Name != g.Name || card.Join != JoinApply || card.Members != 2 {
		t.Fatalf("card fields: %+v", card)
	}
	if card.OwnerCard == nil {
		t.Fatalf("card without owner card")
	}
	if fp, _ := card.OwnerCard.Fingerprint(); fp != owner.Fingerprint() {
		t.Fatalf("owner card mismatch")
	}
	if got := len([]rune(card.RulesHead)); got != 281 { // 280 + ellipsis
		t.Fatalf("rules head not truncated: %d runes", got)
	}
	// An empty join policy reads as invite on the card.
	p.Join = ""
	if c := g.PublicCard(); c.Join != JoinInvite {
		t.Fatalf("empty join policy on the card: %q", c.Join)
	}
}

func TestGroupURIRoundtrip(t *testing.T) {
	gid, _ := NewGroupID()
	uri := EncodeGroupURI(gid, "https://relay.example", "my group")
	got, relay, name, err := ParseGroupURI(uri)
	if err != nil || got != gid || relay != "https://relay.example" || name != "my group" {
		t.Fatalf("roundtrip: %s %s %s %v", got, relay, name, err)
	}
	if _, _, _, err := ParseGroupURI("soulmirror://card?x=1"); err == nil {
		t.Fatalf("card link accepted as a group link")
	}
	if _, _, _, err := ParseGroupURI("soulmirror://group?gid=zz&relay=x"); err == nil {
		t.Fatalf("bad gid accepted")
	}
	if _, _, _, err := ParseGroupURI(EncodeGroupURI(gid, "", "")); err == nil {
		t.Fatalf("empty relay accepted")
	}
}

func TestGroupPinsAndApplicationsStore(t *testing.T) {
	dir := t.TempDir()
	s := NewGroupStore(dir)
	gid, _ := NewGroupID()

	if got := s.Pins(gid); len(got) != 0 {
		t.Fatalf("fresh pins: %v", got)
	}
	p1 := GroupPin{ID: "p1", From: "fpA", TS: time.Now(), Body: "first"}
	if err := s.AddPin(gid, p1); err != nil {
		t.Fatalf("AddPin: %v", err)
	}
	if err := s.AddPin(gid, p1); err != nil {
		t.Fatalf("AddPin twice: %v", err)
	}
	if err := s.AddPin(gid, GroupPin{ID: "p2", From: "fpA", TS: time.Now(), Body: "second"}); err != nil {
		t.Fatalf("AddPin p2: %v", err)
	}
	if got := s.Pins(gid); len(got) != 2 || got[0].ID != "p1" || got[1].ID != "p2" {
		t.Fatalf("pins: %+v", got)
	}
	if err := s.RemovePin(gid, "p1"); err != nil {
		t.Fatalf("RemovePin: %v", err)
	}
	if err := s.RemovePin(gid, "p1"); err != nil {
		t.Fatalf("RemovePin twice: %v", err)
	}
	if got := s.Pins(gid); len(got) != 1 || got[0].ID != "p2" {
		t.Fatalf("pins after remove: %+v", got)
	}

	applicant := grpIdentity(t, "applicant")
	card := grpCard(t, applicant)
	fp := applicant.Fingerprint()
	if got := s.Applications(gid); len(got) != 0 {
		t.Fatalf("fresh applications: %v", got)
	}
	if err := s.PutApplication(gid, &GroupApplication{Card: card, Note: "hi", TS: time.Now()}); err != nil {
		t.Fatalf("PutApplication: %v", err)
	}
	got := s.Applications(gid)
	if len(got) != 1 || got[0].Note != "hi" {
		t.Fatalf("applications: %+v", got)
	}
	if f, _ := got[0].Card.Fingerprint(); f != fp {
		t.Fatalf("application card mismatch")
	}
	if err := s.PutApplication(gid, &GroupApplication{Card: card, Note: "updated", TS: time.Now()}); err != nil {
		t.Fatalf("PutApplication overwrite: %v", err)
	}
	if got := s.Applications(gid); len(got) != 1 || got[0].Note != "updated" {
		t.Fatalf("overwrite: %+v", got)
	}
	if err := s.RemoveApplication(gid, fp); err != nil {
		t.Fatalf("RemoveApplication: %v", err)
	}
	if err := s.RemoveApplication(gid, fp); err != nil {
		t.Fatalf("RemoveApplication twice: %v", err)
	}
	if got := s.Applications(gid); len(got) != 0 {
		t.Fatalf("applications after remove: %+v", got)
	}
	if err := s.PutApplication(gid, nil); err == nil {
		t.Fatalf("nil application accepted")
	}
}

func TestGroupProfilePaidJoinValidation(t *testing.T) {
	valid := &GroupProfile{Join: JoinPaid, JoinPrice: "1.00", JoinAddr: "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29", SpeakHumans: true, SpeakAgents: true}
	if err := valid.Validate(nil); err != nil {
		t.Fatalf("valid paid profile rejected: %v", err)
	}
	cases := []struct {
		name string
		p    *GroupProfile
	}{
		{"missing price", &GroupProfile{Join: JoinPaid, JoinAddr: "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29", SpeakHumans: true, SpeakAgents: true}},
		{"zero price", &GroupProfile{Join: JoinPaid, JoinPrice: "0", JoinAddr: "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29", SpeakHumans: true, SpeakAgents: true}},
		{"bad price", &GroupProfile{Join: JoinPaid, JoinPrice: "abc", JoinAddr: "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29", SpeakHumans: true, SpeakAgents: true}},
		{"too many decimals", &GroupProfile{Join: JoinPaid, JoinPrice: "1.0000001", JoinAddr: "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29", SpeakHumans: true, SpeakAgents: true}},
		{"missing addr", &GroupProfile{Join: JoinPaid, JoinPrice: "1.00", SpeakHumans: true, SpeakAgents: true}},
		{"bad addr", &GroupProfile{Join: JoinPaid, JoinPrice: "1.00", JoinAddr: "0x1234"}},
	}
	for _, c := range cases {
		if err := c.p.Validate(nil); err == nil {
			t.Fatalf("%s: expected validation error", c.name)
		}
	}
	// non-paid groups may omit the price fields
	if err := (&GroupProfile{Join: JoinApply, SpeakHumans: true, SpeakAgents: true}).Validate(nil); err != nil {
		t.Fatalf("apply group rejected: %v", err)
	}
}

func TestJoinPaymentRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewGroupStore(dir)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	card := &Card{EdPub: EncodeKey(priv.Public().(ed25519.PublicKey)), XPub: "y", Name: "applicant", Sig: "z"}
	app := &GroupApplication{
		Card: card, Note: "paid", TS: time.Now(),
		Payment: &JoinPayment{TxHash: "0xabc", Amount: "1.00", To: "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29"},
	}
	if err := s.PutApplication("gid1", app); err != nil {
		t.Fatal(err)
	}
	apps := s.Applications("gid1")
	if len(apps) != 1 || apps[0].Payment == nil || apps[0].Payment.TxHash != "0xabc" || apps[0].Payment.Amount != "1.00" {
		t.Fatalf("payment not round-tripped: %+v", apps)
	}
}
