// Group membership on the light peer (A2A wire spec §14): create groups, join on
// invite, distribute and consume sender keys, send/receive fan-out mail, and rekey on
// member removal. All state lives in a2a.GroupStore (<home>/a2a/groups/<gid>/); the
// conversation archive shares ConvStore under the key a2a.GroupConvKey(gid).
//
// Membership transitions and sender-key epochs (the part that is easy to get subtly
// wrong; every rule below has a test in group_rejoin_test.go):
//
//   - A key distribution carries the chain AT the sender's current index, and the
//     receiver stores it at that index - a late joiner gets a ratcheted chain, not the
//     epoch's origin.
//   - Removing members rotates my sender key to the next epoch AND forgets the receive
//     state of everyone no longer on the roster (a returning member may start over).
//   - Being removed keeps the group on disk read-only (left.json; GroupLeft) - history
//     is the user's data, and my old epoch on file is what makes a re-admission rotate
//     PAST it instead of colliding with what the others still remember.
//   - A same-epoch distribution with different material replaces what I hold: the
//     sender restarted, and only it decides whether its chain decrypts.
//   - An invite for a group I already hold makes me redistribute my key to everyone
//     (I may have been removed and re-added while offline).
package peer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// ErrNoGroup: the peer does not hold this group (never invited, or left).
var ErrNoGroup = fmt.Errorf("unknown group")

// ErrGroupOwner: the operation is reserved for (or forbidden to) the group owner.
var ErrGroupOwner = fmt.Errorf("group owner operation not allowed")

// ErrGroupLeft: the owner removed me from this group. The group and its archive stay on
// this node read-only (see GroupLeft); sending and every fan-out are refused until the
// owner re-admits me.
var ErrGroupLeft = fmt.Errorf("no longer a member of this group")

// ErrPaidProofUsed: the applicant's payment tx has already been consumed by
// another admission — one on-chain payment admits exactly one member.
var ErrPaidProofUsed = fmt.Errorf("payment proof already used")

// GroupSummary is one group row for hosts (list view).
type GroupSummary struct {
	GID      string `json:"gid"`
	Name     string `json:"name"`
	OwnerFp  string `json:"owner_fp"`
	Mine     bool   `json:"mine"` // I am the owner
	Version  int    `json:"version"`
	Members  int    `json:"members"`
	Unread   int    `json:"unread"`
	Count    int    `json:"count"`
	LastTs   int64  `json:"last_ts,omitempty"` // unix ms of the last archived entry
	LastBody string `json:"last_body,omitempty"`
	// Profile is the governance profile off the roster (nil on pre-profile groups), so
	// list rows can gate composers without fetching every group.
	Profile *a2a.GroupProfile `json:"profile,omitempty"`
	// Left: the owner removed me. The group is kept read-only on this node (history
	// intact, no sending, no new mail); it flips back when the owner re-admits me.
	Left bool `json:"left,omitempty"`
}

// GroupMemberView is one member row (fp + display name off the roster card).
type GroupMemberView struct {
	Fp   string `json:"fp"`
	Name string `json:"name"`
	// Agents are the seat-agent names this member announced in the group
	// (TypeGroupVoices metadata; feeds the other members' @-autocomplete).
	Agents []string `json:"agents,omitempty"`
}

// GroupApplicationView is one pending join application (kept on the owner's node only).
type GroupApplicationView struct {
	Fp   string    `json:"fp"`
	Name string    `json:"name"`
	Note string    `json:"note,omitempty"`
	TS   time.Time `json:"ts"`
	// Payment is the paid-join proof, when the group's join policy is "paid".
	Payment *a2a.JoinPayment `json:"payment,omitempty"`
}

// GroupMemberPreview is how many members GroupInfoMembers carries by default: a large
// roster must not be shipped in full on every poll of the info view (the full list and
// search go through GroupMembers).
const GroupMemberPreview = 50

// GroupMembersMaxPage caps one page of GroupMembers / GroupInvitable.
const GroupMembersMaxPage = 200

// GroupView is one group in full (info view).
type GroupView struct {
	GroupSummary
	// MemberList is the member preview in display order (owner → admins → the rest).
	// GroupInfo carries every member; GroupInfoMembers carries `preview` of them and sets
	// MemberTruncated when it cut the list.
	MemberList []GroupMemberView `json:"member_list"`
	// MemberTruncated marks MemberList as a preview (see GroupInfoMembers).
	MemberTruncated bool `json:"member_truncated,omitempty"`
	// Voices maps member fingerprint → the seat-agent names that member announced
	// (group-wide, independent of the member preview; only members with seats appear).
	Voices map[string][]string `json:"voices,omitempty"`
	// MyFp is my own fingerprint (UIs exclude it from member pickers).
	MyFp string `json:"my_fp,omitempty"`
	// URI is the group handle (soulmirror://group?…) strangers use to look the group up
	// and apply; whether sharing it makes sense depends on the join policy.
	URI string `json:"uri,omitempty"`
	// Pins are the pinned announcements (oldest first).
	Pins []a2a.GroupPin `json:"pins,omitempty"`
	// MyRole is my standing in this group: owner | admin | member.
	MyRole string `json:"my_role"`
	// Applications are the pending join applications (owner's node only).
	Applications []GroupApplicationView `json:"applications,omitempty"`
}

// GroupSummaryOf builds the list row of one group state (counts, unread, last entry,
// removed marker) without touching the member cards - cheap enough for a large roster.
func (n *Peer) GroupSummaryOf(st *a2a.GroupState) GroupSummary {
	gid := st.Roster.GroupID
	count, last, unread := n.Convs.Summary(a2a.GroupConvKey(gid), st.LastReadAt)
	s := GroupSummary{GID: gid, Name: st.Roster.Name, OwnerFp: st.Roster.OwnerFp(),
		Mine: st.Roster.OwnerFp() == n.Fingerprint(), Version: st.Roster.Version,
		Members: len(st.Roster.Members), Unread: unread, Count: count, Profile: st.Roster.Profile,
		Left: n.GroupLeft(gid)}
	if last != nil {
		s.LastTs = last.TS.UnixMilli()
		s.LastBody = last.Body
	}
	return s
}

// rosterIndex is a per-roster-version cache: fingerprint → card plus the display order
// (owner → admins → the rest, roster order within each). Fingerprinting a thousand cards
// (base64 + SHA-256) on every info poll is what this avoids; a roster only changes with
// its version.
type rosterIndex struct {
	version int
	n       int
	ownerFp string
	byFp    map[string]*a2a.Card
	order   []string
}

func (n *Peer) rosterIndexOf(st *a2a.GroupState) *rosterIndex {
	gid := st.Roster.GroupID
	n.riMu.Lock()
	defer n.riMu.Unlock()
	if n.rosterIdx == nil {
		n.rosterIdx = map[string]*rosterIndex{}
	}
	if ix := n.rosterIdx[gid]; ix != nil && ix.version == st.Roster.Version && ix.n == len(st.Roster.Members) {
		return ix
	}
	ownerFp := st.Roster.OwnerFp()
	ix := &rosterIndex{version: st.Roster.Version, n: len(st.Roster.Members), ownerFp: ownerFp,
		byFp: make(map[string]*a2a.Card, len(st.Roster.Members))}
	var owner, admins, rest []string
	for _, c := range st.Roster.Members {
		fp, err := c.Fingerprint()
		if err != nil {
			continue
		}
		if _, dup := ix.byFp[fp]; dup {
			continue
		}
		ix.byFp[fp] = c
		switch {
		case fp == ownerFp:
			owner = append(owner, fp)
		case st.Roster.Profile.IsAdmin(fp):
			admins = append(admins, fp)
		default:
			rest = append(rest, fp)
		}
	}
	ix.order = append(append(append(make([]string, 0, len(ix.byFp)), owner...), admins...), rest...)
	n.rosterIdx[gid] = ix
	return ix
}

// friendNotes loads every friend note once (FriendStore.Get parses friends.yaml on each
// call; per-member lookups on a large roster would be thousands of reads).
func (n *Peer) friendNotes() map[string]string {
	out := map[string]string{}
	for _, fr := range n.Friends.Friends() {
		if fr != nil && strings.TrimSpace(fr.Note) != "" {
			out[fr.Fingerprint] = strings.TrimSpace(fr.Note)
		}
	}
	return out
}

// memberName is the display-name rule for one member: my friend note for them beats the
// name on their roster card; I am shown under my own identity name.
func memberName(c *a2a.Card, fp string, notes map[string]string, me, myName string) string {
	name := ""
	if c != nil {
		name = c.Name
	}
	if v := notes[fp]; v != "" {
		name = v
	}
	if fp == me && myName != "" {
		name = myName
	}
	return name
}

// groupView builds the info view. preview < 0 carries every member, 0 none, otherwise the
// first preview members in display order (MemberTruncated set when cut).
func (n *Peer) groupView(st *a2a.GroupState, preview int) *GroupView {
	v := &GroupView{GroupSummary: n.GroupSummaryOf(st)}
	me, myName := "", ""
	if id := n.Identity(); id != nil {
		me, myName = id.Fingerprint(), id.Name
	}
	v.MyFp = me
	ix := n.rosterIndexOf(st)
	switch {
	case ix.ownerFp == me:
		v.MyRole = "owner"
	case st.Roster.Profile.IsAdmin(me):
		v.MyRole = "admin"
	default:
		v.MyRole = "member"
	}
	gid := st.Roster.GroupID
	v.Pins = n.Groups.Pins(gid)
	if v.MyRole == "owner" {
		v.Applications = n.groupApplicationViews(gid)
	}
	v.Voices = n.GroupVoices(gid)
	v.URI = a2a.EncodeGroupURI(gid, st.Roster.Relay, st.Roster.Name)
	count := len(ix.order)
	if preview >= 0 && preview < count {
		count = preview
		v.MemberTruncated = true
	}
	notes := n.friendNotes()
	v.MemberList = make([]GroupMemberView, 0, count)
	for _, fp := range ix.order[:count] {
		v.MemberList = append(v.MemberList, GroupMemberView{Fp: fp,
			Name: memberName(ix.byFp[fp], fp, notes, me, myName), Agents: v.Voices[fp]})
	}
	return v
}

// GroupMembersPage is one page of GroupMembers / GroupInvitable.
type GroupMembersPage struct {
	Members []GroupMemberView `json:"members"`
	// Total is the group size (GroupInvitable: the number of candidate friends).
	Total int `json:"total"`
	// Matched is how many rows matched the query (== Total for an empty query).
	Matched int `json:"matched"`
	Offset  int `json:"offset"`
	Limit   int `json:"limit"`
}

// groupMemberMatch: the (lower-cased) query is contained in the name, prefixes the
// fingerprint, or is contained in one of the announced seat names.
func groupMemberMatch(name, fp string, agents []string, q string) bool {
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(name), q) || strings.HasPrefix(strings.ToLower(fp), q) {
		return true
	}
	for _, a := range agents {
		if strings.Contains(strings.ToLower(a), q) {
			return true
		}
	}
	return false
}

func groupPageSlice(items []GroupMemberView, offset, limit int) *GroupMembersPage {
	matched := len(items)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > GroupMembersMaxPage {
		limit = GroupMembersMaxPage
	}
	if offset > matched {
		offset = matched
	}
	end := offset + limit
	if end > matched {
		end = matched
	}
	return &GroupMembersPage{Members: items[offset:end], Matched: matched, Offset: offset, Limit: limit}
}

// GroupInfoMembers is GroupInfo with a member preview: preview < 0 = every member, 0 =
// none (names then come from GroupNames), otherwise the first preview members.
func (n *Peer) GroupInfoMembers(gid string, preview int) (*GroupView, error) {
	st := n.Groups.Get(gid)
	if st == nil {
		return nil, ErrNoGroup
	}
	return n.groupView(st, preview), nil
}

// GroupNames resolves fingerprints to their display names in one group (friend note
// beats roster name; unknown fingerprints are skipped). nil when nothing resolved.
func (n *Peer) GroupNames(gid string, fps []string) map[string]string {
	st := n.Groups.Get(gid)
	if st == nil || len(fps) == 0 {
		return nil
	}
	ix := n.rosterIndexOf(st)
	notes := n.friendNotes()
	me, myName := "", ""
	if id := n.Identity(); id != nil {
		me, myName = id.Fingerprint(), id.Name
	}
	out := map[string]string{}
	for _, fp := range fps {
		if fp == "" {
			continue
		}
		if _, done := out[fp]; done {
			continue
		}
		if name := memberName(ix.byFp[fp], fp, notes, me, myName); name != "" {
			out[fp] = name
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GroupMembers pages / searches the members of one group in display order (owner →
// admins → the rest). An empty q pages the whole roster.
func (n *Peer) GroupMembers(gid, q string, offset, limit int) (*GroupMembersPage, error) {
	st := n.Groups.Get(gid)
	if st == nil {
		return nil, ErrNoGroup
	}
	ix := n.rosterIndexOf(st)
	notes := n.friendNotes()
	voices := n.GroupVoices(gid)
	me, myName := "", ""
	if id := n.Identity(); id != nil {
		me, myName = id.Fingerprint(), id.Name
	}
	q = strings.ToLower(strings.TrimSpace(q))
	hits := make([]GroupMemberView, 0, 64)
	for _, fp := range ix.order {
		name := memberName(ix.byFp[fp], fp, notes, me, myName)
		if !groupMemberMatch(name, fp, voices[fp], q) {
			continue
		}
		hits = append(hits, GroupMemberView{Fp: fp, Name: name, Agents: voices[fp]})
	}
	page := groupPageSlice(hits, offset, limit)
	page.Total = len(ix.order)
	return page, nil
}

// GroupInvitable pages / searches my friends who are NOT in the group yet - the
// candidates for an invite (a client that only holds a member preview cannot compute
// this intersection itself).
func (n *Peer) GroupInvitable(gid, q string, offset, limit int) (*GroupMembersPage, error) {
	st := n.Groups.Get(gid)
	if st == nil {
		return nil, ErrNoGroup
	}
	ix := n.rosterIndexOf(st)
	q = strings.ToLower(strings.TrimSpace(q))
	all, hits := 0, make([]GroupMemberView, 0, 32)
	for _, fr := range n.Friends.Friends() {
		if fr == nil || ix.byFp[fr.Fingerprint] != nil {
			continue
		}
		all++
		name := strings.TrimSpace(fr.Note)
		if name == "" && fr.Card != nil {
			name = fr.Card.Name
		}
		if !groupMemberMatch(name, fr.Fingerprint, nil, q) {
			continue
		}
		hits = append(hits, GroupMemberView{Fp: fr.Fingerprint, Name: name})
	}
	page := groupPageSlice(hits, offset, limit)
	page.Total = all
	return page, nil
}

// groupRelayClient talks to the group's home relay.
func (n *Peer) groupRelayClient(relay string) *a2a.ProxyClient {
	return a2a.NewProxyClient(strings.TrimRight(relay, "/"), n.Identity()).WithDeliverTimeout(DeliverTimeout)
}

// sendGroupPairwise seals a pairwise group_* message to an arbitrary card (members need
// not be friends) with from_xpub declared, delivering now or queueing in the outbox.
func (n *Peer) sendGroupPairwise(ctx context.Context, card *a2a.Card, msg *a2a.Message) error {
	env, err := n.seal(card, msg)
	if err != nil {
		return err
	}
	env.FromXPub = n.Identity().XPub // the receiver may not hold our card (co-member, not friend)
	if err := n.DeliverToCard(ctx, card, env); err != nil {
		n.logf("group pairwise delivery failed (queued): %v", err)
		if qerr := n.queueOutbox(card, env); qerr != nil {
			return fmt.Errorf("delivery failed and could not be queued: %v / %v", err, qerr)
		}
	}
	return nil
}

// distributeSenderKey sends MY current chain key for gid to every co-member who has not
// received this epoch yet, and records the successful distributions. Best effort per
// member; a no-op without a sender key. Network sends happen OUTSIDE gkMu; the record
// is written against a fresh read so a concurrent chain advance is never clobbered.
func (n *Peer) distributeSenderKey(ctx context.Context, st *a2a.GroupState) {
	me := n.Fingerprint()
	gid := st.Roster.GroupID
	n.gkMu.Lock()
	keys := n.Groups.Keys(gid)
	if keys.Mine == nil {
		n.gkMu.Unlock()
		return
	}
	epoch, index, chain := keys.Mine.Epoch, keys.Mine.Index, keys.Mine.Chain
	targets := map[string]*a2a.Card{}
	for _, c := range st.Roster.Members {
		fp, err := c.Fingerprint()
		if err != nil || fp == me || keys.DistributedTo[fp] >= epoch {
			continue
		}
		targets[fp] = c
	}
	n.gkMu.Unlock()
	if len(targets) == 0 {
		return
	}
	var reached []string
	for fp, c := range targets {
		msg := &a2a.Message{ID: n.newMsgID(), From: me, To: fp, TS: time.Now(),
			Type: a2a.TypeGroupKey, GID: gid, GKey: &a2a.GroupKeyDist{Epoch: epoch, Index: index, Chain: chain}}
		if err := n.sendGroupPairwise(ctx, c, msg); err != nil {
			n.logf("group %s: key distribution to %s failed: %v", a2a.ShortFp(gid), a2a.ShortFp(fp), err)
			continue
		}
		reached = append(reached, fp)
	}
	if len(reached) == 0 {
		return
	}
	n.gkMu.Lock()
	defer n.gkMu.Unlock()
	fresh := n.Groups.Keys(gid)
	if fresh.Mine == nil || fresh.Mine.Epoch != epoch {
		return // rekeyed meanwhile: the new epoch redistributes on its own
	}
	for _, fp := range reached {
		if fresh.DistributedTo[fp] < epoch {
			fresh.DistributedTo[fp] = epoch
		}
	}
	if err := n.Groups.PutKeys(gid, fresh); err != nil {
		n.logf("group %s: persisting key distribution: %v", a2a.ShortFp(gid), err)
	}
}

// ensureSenderKey initializes my sending chain for gid (epoch 1) when absent.
func (n *Peer) ensureSenderKey(gid string) error {
	n.gkMu.Lock()
	defer n.gkMu.Unlock()
	keys := n.Groups.Keys(gid)
	if keys.Mine != nil {
		return nil
	}
	var err error
	if keys.Mine, err = a2a.NewGroupSenderKey(1); err != nil {
		return err
	}
	return n.Groups.PutKeys(gid, keys)
}

// rekeyGroup replaces MY sender key with a fresh chain at the next epoch and
// redistributes it to the current members. Called when members were removed (they keep
// the old chain, so a new one shuts them out) and when the owner re-admits me (a member
// that still holds my old receive state would take a same-epoch chain for a replay; a
// higher epoch is accepted by everyone).
//
// It also forgets the receive state and distribution record of everyone who is no longer
// on the roster. A removed member that comes back may restart its chain at epoch 1 /
// index 0 (older peers wipe the group when kicked); with its previous {epoch 1, index k}
// still on file, handleGroupKey would treat the new chain as a replay and every message
// from it would fail with "index 0 already consumed". State for non-members has no
// value worth keeping.
func (n *Peer) rekeyGroup(ctx context.Context, st *a2a.GroupState, why string) {
	gid := st.Roster.GroupID
	n.gkMu.Lock()
	keys := n.Groups.Keys(gid)
	epoch := 1
	if keys.Mine != nil {
		epoch = keys.Mine.Epoch + 1
	}
	fresh, err := a2a.NewGroupSenderKey(epoch)
	if err != nil {
		n.gkMu.Unlock()
		n.logf("group %s: rekey failed: %v", a2a.ShortFp(gid), err)
		return
	}
	keys.Mine = fresh
	keys.DistributedTo = map[string]int{}
	pruneGroupKeysToRoster(keys, st.Roster)
	err = n.Groups.PutKeys(gid, keys)
	n.gkMu.Unlock()
	if err != nil {
		n.logf("group %s: persisting rekey: %v", a2a.ShortFp(gid), err)
		return
	}
	n.logf("group %s: sender key rotated to epoch %d (%s)", a2a.ShortFp(gid), epoch, why)
	n.distributeSenderKey(ctx, st)
}

// pruneGroupKeysToRoster drops receive states and distribution records of fingerprints
// that are not on the roster (caller holds gkMu).
func pruneGroupKeysToRoster(keys *a2a.GroupKeys, roster *a2a.GroupRoster) {
	for fp := range keys.Senders {
		if roster.Member(fp) == nil {
			delete(keys.Senders, fp)
		}
	}
	for fp := range keys.DistributedTo {
		if roster.Member(fp) == nil {
			delete(keys.DistributedTo, fp)
		}
	}
}

// redistributeSenderKey hands my CURRENT chain to every co-member again, ignoring the
// distribution record. Used when an invite arrives for a group I already hold: the owner
// is (re)admitting me, and I may have been removed and re-added while offline - no
// roster diff ever told me, but the others forgot my key when they rekeyed.
func (n *Peer) redistributeSenderKey(ctx context.Context, st *a2a.GroupState) {
	gid := st.Roster.GroupID
	n.gkMu.Lock()
	keys := n.Groups.Keys(gid)
	if keys.Mine == nil {
		n.gkMu.Unlock()
		return
	}
	keys.DistributedTo = map[string]int{}
	err := n.Groups.PutKeys(gid, keys)
	n.gkMu.Unlock()
	if err != nil {
		n.logf("group %s: resetting key distribution: %v", a2a.ShortFp(gid), err)
		return
	}
	n.distributeSenderKey(ctx, st)
}

// GroupCreate creates a group with me as owner plus the given FRIENDS, publishes the
// signed roster on my relay and invites every member (roster + my sender key, pairwise).
// A nil profile means the "standard" default (a2a.DefaultGroupProfile); the profile is
// signed into the roster.
func (n *Peer) GroupCreate(ctx context.Context, name string, memberFps []string, profile *a2a.GroupProfile) (*GroupView, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	ctx = ctxOrBackground(ctx)
	id := n.Identity()
	myCard, err := id.Card()
	if err != nil {
		return nil, err
	}
	cards := []*a2a.Card{myCard}
	me := n.Fingerprint()
	seen := map[string]bool{me: true}
	for _, fp := range memberFps {
		fp = strings.TrimSpace(fp)
		if fp == "" || seen[fp] {
			continue
		}
		fr := n.Friends.Get(fp)
		if fr == nil || fr.Card == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFriend, a2a.ShortFp(fp))
		}
		cards = append(cards, fr.Card)
		seen[fp] = true
	}
	if len(cards) < 2 {
		return nil, fmt.Errorf("a group needs at least one member besides the owner")
	}
	if profile == nil {
		profile = a2a.DefaultGroupProfile()
	}
	fps := make([]string, 0, len(cards))
	for _, c := range cards {
		if f, err := c.Fingerprint(); err == nil {
			fps = append(fps, f)
		}
	}
	if err := profile.Validate(fps); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadProfile, err)
	}
	gid, err := a2a.NewGroupID()
	if err != nil {
		return nil, err
	}
	roster := &a2a.GroupRoster{V: 1, GroupID: gid, Name: strings.TrimSpace(name),
		OwnerPub: id.EdPub, Relay: n.RelayBase(), Version: 1, Members: cards, TS: time.Now(),
		Profile: profile}
	priv, err := id.EdPrivate()
	if err != nil {
		return nil, err
	}
	roster.Sign(priv)
	if err := roster.Verify(); err != nil {
		return nil, err
	}
	if err := n.groupRelayClient(roster.Relay).PublishGroup(ctx, roster); err != nil {
		return nil, fmt.Errorf("%w: publishing the roster: %v", ErrNetwork, err)
	}
	st := &a2a.GroupState{Roster: roster, JoinedAt: time.Now(), LastReadAt: time.Now()}
	if err := n.Groups.Put(st); err != nil {
		return nil, err
	}
	if err := n.ensureSenderKey(gid); err != nil {
		return nil, err
	}
	// Invite every member: the signed roster (join ticket) + my sender key.
	for _, c := range cards[1:] {
		fp, _ := c.Fingerprint()
		invite := &a2a.Message{ID: n.newMsgID(), From: me, To: fp, TS: time.Now(),
			Type: a2a.TypeGroupInvite, GID: gid, Group: roster, Body: roster.Name}
		if err := n.sendGroupPairwise(ctx, c, invite); err != nil {
			n.logf("group %s: invite to %s failed: %v", a2a.ShortFp(gid), a2a.ShortFp(fp), err)
		}
	}
	n.distributeSenderKey(ctx, st)
	n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonCreated})
	return n.groupView(st, -1), nil
}

// GroupList returns every group I am in.
func (n *Peer) GroupList() []GroupSummary {
	states := n.Groups.List()
	out := make([]GroupSummary, 0, len(states))
	for _, st := range states {
		out = append(out, n.GroupSummaryOf(st))
	}
	return out
}

// GroupInfo returns one group in full (every member; see GroupInfoMembers for a preview).
func (n *Peer) GroupInfo(gid string) (*GroupView, error) { return n.GroupInfoMembers(gid, -1) }

// fanOutGroup seals msg once with my sender chain and posts ONE group envelope for the
// relay to fan out. Missing key distributions are topped up first, and the advanced
// chain is persisted BEFORE delivering (a chain position must never be reused).
func (n *Peer) fanOutGroup(ctx context.Context, st *a2a.GroupState, msg *a2a.Message) error {
	gid := st.Roster.GroupID
	if n.GroupLeft(gid) {
		return ErrGroupLeft // read-only: nothing goes out (the relay would refuse it anyway)
	}
	if err := n.ensureSenderKey(gid); err != nil {
		return err
	}
	n.distributeSenderKey(ctx, st)
	n.gkMu.Lock()
	keys := n.Groups.Keys(gid)
	cipher, err := a2a.GroupSeal(keys.Mine, msg)
	if err == nil {
		err = n.Groups.PutKeys(gid, keys)
	}
	n.gkMu.Unlock()
	if err != nil {
		return err
	}
	env, err := a2a.SealGroupEnvelope(n.Identity(), gid, cipher)
	if err != nil {
		return err
	}
	return n.groupRelayClient(st.Roster.Relay).DeliverGroup(ctx, env)
}

// GroupSendOptions are the optional parts of one group send.
type GroupSendOptions struct {
	// By is the provenance of the post: a2a.ByOwner (the human; "" counts as owner) or
	// a2a.ByAlter (the agent). The roster profile's speak switches are enforced
	// against it before sending.
	By string
	// Auto marks the alter's automatic posts (loop guard, like pairwise sends).
	Auto bool
	// Agent names which of this seat's agents composed a by=alter post (display
	// provenance, e.g. "DevBot"; empty = the default alter). Governance reads By.
	Agent string
}

// GroupSend encrypts body once with my sender chain and posts ONE group envelope; the
// relay fans it out to the other members. The roster profile is enforced locally first
// (AllowSpeak); every receiver enforces it again on its side.
func (n *Peer) GroupSend(ctx context.Context, gid, body string, opts GroupSendOptions) (*SendResult, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return nil, ErrNoGroup
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("%w: body must not be empty", ErrBadFile)
	}
	if n.GroupLeft(gid) {
		return nil, ErrGroupLeft
	}
	me := n.Fingerprint()
	if err := st.Roster.AllowSpeak(me, opts.By); err != nil {
		return nil, err
	}
	ctx = ctxOrBackground(ctx)
	msg := &a2a.Message{ID: n.newMsgID(), From: me, GID: gid, TS: time.Now(), Type: a2a.TypeText,
		Body: body, By: opts.By, Auto: opts.Auto, Agent: opts.Agent}
	res := &SendResult{ID: msg.ID, Status: "sent"}
	if err := n.fanOutGroup(ctx, st, msg); err != nil {
		n.logf("group %s: fan-out delivery failed: %v", a2a.ShortFp(gid), err)
		res.Status = "error"
		go n.refreshRoster(context.Background(), gid) // maybe the roster moved on without us
	} else {
		n.logf("<<< group mail gid=%s id=%s by=%s", a2a.ShortFp(gid), msg.ID, msg.By)
	}
	seq, err := n.Convs.AppendSeq(a2a.GroupConvKey(gid), &a2a.ConvEntry{Dir: "out", Message: *msg, Status: res.Status})
	if err != nil {
		return nil, err
	}
	res.Seq = seq
	return res, nil
}

// GroupTyping fans out a "this seat is working here" signal (agent = which of my
// seat agents, "" = the alter): presence metadata like pairwise typing — never
// archived, AllowSpeak does not apply, best effort.
func (n *Peer) GroupTyping(ctx context.Context, gid string, on bool, agent string) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	body := "on"
	if !on {
		body = "off"
	}
	msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), GID: gid, TS: time.Now(),
		Type: a2a.TypeTyping, Body: body, Agent: agent}
	if err := n.fanOutGroup(ctxOrBackground(ctx), st, msg); err != nil {
		n.logf("group %s: typing signal delivery failed: %v", a2a.ShortFp(gid), err)
	}
	return nil
}

// GroupVoicesSyncBody is the Body of a voices announcement that also asks every member to
// re-announce theirs ("this is my list - please replay yours"; a fresh node holds nobody's
// list). Peers that predate it read only Voices and consume the message as a plain
// announcement, which is semantically correct - the wire format is unchanged.
const GroupVoicesSyncBody = "sync"

// GroupVoicesOptions are the optional parts of one voices announcement.
type GroupVoicesOptions struct {
	// Sync also asks every member to re-announce their own seat names
	// (Body = GroupVoicesSyncBody). Receivers report it as group.updated / voices with
	// the announcement in Message; whether to answer is the host's call.
	Sync bool
}

func (n *Peer) voicesMessage(gid string, voices []string, sync bool) *a2a.Message {
	msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), GID: gid, TS: time.Now(),
		Type: a2a.TypeGroupVoices, Voices: SanitizeVoices(voices)}
	if sync {
		msg.Body = GroupVoicesSyncBody
	}
	return msg
}

// GroupAnnounceVoicesWith is GroupAnnounceVoices with options and WITHOUT the built-in
// delayed retry: the delivery error is returned as is, for hosts that run their own
// self-healing schedule (replay on start, answer sync requests, periodic top-up) and
// want to throttle and retry themselves.
func (n *Peer) GroupAnnounceVoicesWith(ctx context.Context, gid string, voices []string, opts GroupVoicesOptions) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	return n.fanOutGroup(ctxOrBackground(ctx), st, n.voicesMessage(gid, voices, opts.Sync))
}

// GroupAnnounceVoices fans out this seat's enabled agent names in one group
// (TypeGroupVoices): metadata for the other members' @-autocomplete, not speech
// (AllowSpeak does not apply) and never archived. An empty list clears the entry.
func (n *Peer) GroupAnnounceVoices(ctx context.Context, gid string, voices []string) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	msg := n.voicesMessage(gid, voices, false)
	if err := n.fanOutGroup(ctxOrBackground(ctx), st, msg); err != nil {
		// Startup announces hit the relay in a burst and an occasional post
		// times out; the roster metadata must still converge - one delayed
		// retry covers it (receivers dedupe by message id).
		n.logf("group %s: voices announce delivery failed (retrying once): %v", a2a.ShortFp(gid), err)
		go func() {
			time.Sleep(7 * time.Second)
			if st2 := n.Groups.Get(gid); st2 != nil {
				if err2 := n.fanOutGroup(context.Background(), st2, msg); err2 != nil {
					n.logf("group %s: voices announce retry failed: %v", a2a.ShortFp(gid), err2)
				}
			}
		}()
	}
	return nil
}

// SanitizeVoices caps and trims an announced seat-agent name list (self-declared data,
// applied on both the sending and the receiving side): at most 16 names, each non-empty,
// at most 64 bytes and free of whitespace / "@" / "·" (the @-mention delimiters).
func SanitizeVoices(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > 64 || strings.ContainsAny(v, "@·\n\r\t ") {
			continue
		}
		out = append(out, v)
		if len(out) == 16 {
			break
		}
	}
	return out
}

// ——— seat-agent voices roster (groups/<gid>/voices.json) ———
//
// Who announced which seat agents in a group is metadata the OTHER members sent us; kept
// in memory only it would vanish on every restart and the announcing seat would drop out
// of everyone's @-completion until it happens to re-announce. It is persisted next to
// the group state (removed with the group), re-sanitised on load (self-declared data:
// names through SanitizeVoices, only current roster members, at most MaxVoiceMembers
// entries), and written only when an announcement actually changed something.

// MaxVoiceMembers caps the persisted entries of one group's voices roster (a hard ceiling
// on top of the roster size, so a huge group with every seat announcing cannot fill the
// disk).
const MaxVoiceMembers = 2000

func (n *Peer) voicesPath(gid string) string {
	return filepath.Join(n.Home, "a2a", "groups", a2a.SanitizeID(gid), "voices.json")
}

// loadVoices reads and sanitises one group's roster from disk (never nil; a broken file
// degrades to empty - the next announcements rebuild it).
func (n *Peer) loadVoices(gid string) map[string][]string {
	out := map[string][]string{}
	raw, err := os.ReadFile(n.voicesPath(gid))
	if err != nil {
		return out
	}
	var disk map[string][]string
	if json.Unmarshal(raw, &disk) != nil {
		n.logf("group %s: voices roster unreadable (starting empty)", a2a.ShortFp(gid))
		return out
	}
	var roster *a2a.GroupRoster
	if st := n.Groups.Get(gid); st != nil {
		roster = st.Roster
	}
	fps := make([]string, 0, len(disk))
	for fp := range disk {
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	for _, fp := range fps {
		if roster != nil && roster.Member(fp) == nil {
			continue
		}
		names := SanitizeVoices(disk[fp])
		if len(names) == 0 {
			continue
		}
		out[fp] = names
		if len(out) >= MaxVoiceMembers {
			break
		}
	}
	return out
}

// voicesLocked returns the in-memory roster of gid, loading it from disk on first use
// (caller holds gvMu).
func (n *Peer) voicesLocked(gid string) map[string][]string {
	if n.groupVoices == nil {
		n.groupVoices = map[string]map[string][]string{}
	}
	if m, ok := n.groupVoices[gid]; ok {
		return m
	}
	m := n.loadVoices(gid)
	n.groupVoices[gid] = m
	return m
}

// writeVoicesLocked persists one group's roster (caller holds gvMu); empty = file removed.
func (n *Peer) writeVoicesLocked(gid string, m map[string][]string) {
	path := n.voicesPath(gid)
	if len(m) == 0 {
		_ = os.Remove(path)
		return
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return // no group directory (not joined): nothing to persist against
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		n.logf("group %s: persisting voices roster: %v", a2a.ShortFp(gid), err)
	}
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// setGroupVoices records one member's announced seat names (empty = entry removed);
// memory and disk stay in step, unchanged announcements do not touch the disk.
func (n *Peer) setGroupVoices(gid, fp string, voices []string) {
	clean := SanitizeVoices(voices)
	n.gvMu.Lock()
	defer n.gvMu.Unlock()
	m := n.voicesLocked(gid)
	if len(clean) == 0 {
		if _, had := m[fp]; !had {
			return
		}
		delete(m, fp)
	} else {
		if sameNames(m[fp], clean) {
			return
		}
		if _, had := m[fp]; !had && len(m) >= MaxVoiceMembers {
			n.logf("group %s: voices roster full (%d entries), dropping the announcement from %s", a2a.ShortFp(gid), MaxVoiceMembers, a2a.ShortFp(fp))
			return
		}
		m[fp] = clean
	}
	n.writeVoicesLocked(gid, m)
}

// SetGroupVoices records (or, with an empty list, clears) one member's seat-agent names
// in a group's voices roster, exactly as an incoming announcement would. Hosts use it to
// seed or prune the roster from their own knowledge; the receive path calls the same code.
func (n *Peer) SetGroupVoices(gid, fp string, voices []string) { n.setGroupVoices(gid, fp, voices) }

// GroupVoices returns a copy of one group's voices roster: member fingerprint → the seat
// agent names they announced (nil when nobody announced any).
func (n *Peer) GroupVoices(gid string) map[string][]string {
	n.gvMu.Lock()
	defer n.gvMu.Unlock()
	src := n.voicesLocked(gid)
	if len(src) == 0 {
		return nil
	}
	out := make(map[string][]string, len(src))
	for fp, names := range src {
		out[fp] = append([]string(nil), names...)
	}
	return out
}

// voicesOf answers one member's announced agent names in a group (nil = none known).
func (n *Peer) voicesOf(gid, fp string) []string {
	n.gvMu.Lock()
	defer n.gvMu.Unlock()
	v := n.voicesLocked(gid)[fp]
	if len(v) == 0 {
		return nil
	}
	return append([]string(nil), v...)
}

// GroupConversation returns the archived entries of one group.
func (n *Peer) GroupConversation(gid string, sinceSeq, limit int) []Entry {
	return n.Conversation(a2a.GroupConvKey(gid), sinceSeq, limit)
}

// GroupMarkRead moves the group read cursor to the timestamp of line seq (<=0 = now).
func (n *Peer) GroupMarkRead(gid string, seq int) error {
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	t := time.Now()
	key := a2a.GroupConvKey(gid)
	if seq > 0 {
		if es := n.Convs.Since(key, seq-1, 0); len(es) > 0 && es[0].Seq == seq {
			t = es[0].TS
		}
	}
	return n.Groups.MarkRead(gid, t)
}

// GroupLeave: tell the owner to drop me, then forget the group locally (the archive is
// kept). The owner cannot leave — a group lives and dies with its owner for now.
func (n *Peer) GroupLeave(ctx context.Context, gid string) error {
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	me := n.Fingerprint()
	if st.Roster.OwnerFp() == me {
		return fmt.Errorf("%w: the owner cannot leave its own group", ErrGroupOwner)
	}
	if ownerCard := st.Roster.Member(st.Roster.OwnerFp()); ownerCard != nil && !n.GroupLeft(gid) {
		// Already removed by the owner = the user deleting the read-only record; nothing
		// to tell the owner then.
		leave := &a2a.Message{ID: n.newMsgID(), From: me, To: st.Roster.OwnerFp(), TS: time.Now(),
			Type: a2a.TypeGroupLeave, GID: gid}
		if err := n.sendGroupPairwise(ctxOrBackground(ctx), ownerCard, leave); err != nil {
			n.logf("group %s: leave notice failed: %v", a2a.ShortFp(gid), err)
		}
	}
	if err := n.Groups.Remove(gid); err != nil {
		return err
	}
	n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonLeft})
	return nil
}

// GroupKick removes a member. The owner republishes the roster without them (every
// remaining member rekeys); an admin (per the roster profile) forwards a `group_admin`
// kick request to the owner, whose node executes it mechanically; anyone else is refused.
func (n *Peer) GroupKick(ctx context.Context, gid, fp string) error {
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	me := n.Fingerprint()
	if fp == me {
		return fmt.Errorf("%w: cannot remove yourself (use leave)", ErrGroupOwner)
	}
	if st.Roster.Member(fp) == nil {
		return fmt.Errorf("%s is not a member", a2a.ShortFp(fp))
	}
	ctx = ctxOrBackground(ctx)
	if st.Roster.OwnerFp() == me {
		return n.republishRoster(ctx, st, []string{fp}, nil, nil)
	}
	if st.Roster.Profile.IsAdmin(me) {
		if fp == st.Roster.OwnerFp() {
			return fmt.Errorf("%w: the owner cannot be kicked", ErrGroupOwner)
		}
		ownerCard := st.Roster.Member(st.Roster.OwnerFp())
		if ownerCard == nil {
			return fmt.Errorf("roster has no owner card")
		}
		req := &a2a.Message{ID: n.newMsgID(), From: me, To: st.Roster.OwnerFp(), TS: time.Now(),
			Type: a2a.TypeGroupAdmin, GID: gid, Body: "kick " + fp}
		return n.sendGroupPairwise(ctx, ownerCard, req)
	}
	return fmt.Errorf("%w: only the owner or an admin can remove members", ErrGroupOwner)
}

// republishRoster is the owner-side roster change used by kick/leave/approve/invite/
// setProfile: drop and/or add members, optionally replace the profile, bump the version,
// sign, publish, then converge everyone — dropped members trigger a rekey and get a
// pairwise removal notice, added members get a pairwise group_invite plus my sender key,
// and the group is poked (group_update fan-out) to refetch.
func (n *Peer) republishRoster(ctx context.Context, st *a2a.GroupState, drop []string, add []*a2a.Card, newProfile *a2a.GroupProfile) error {
	id := n.Identity()
	old := st.Roster
	dropSet := map[string]bool{}
	for _, fp := range drop {
		dropSet[fp] = true
	}
	next := &a2a.GroupRoster{V: 1, GroupID: old.GroupID, Name: old.Name, OwnerPub: old.OwnerPub,
		Relay: old.Relay, Version: old.Version + 1, TS: time.Now()}
	present := map[string]bool{}
	var droppedCards []*a2a.Card
	for _, c := range old.Members {
		f, err := c.Fingerprint()
		if err != nil {
			continue
		}
		if dropSet[f] {
			droppedCards = append(droppedCards, c)
			continue
		}
		next.Members = append(next.Members, c)
		present[f] = true
	}
	var addedCards []*a2a.Card
	for _, c := range add {
		if c == nil {
			continue
		}
		f, err := c.Fingerprint()
		if err != nil {
			return err
		}
		if present[f] {
			continue
		}
		next.Members = append(next.Members, c)
		present[f] = true
		addedCards = append(addedCards, c)
	}
	profile := old.Profile
	if newProfile != nil {
		profile = newProfile
	}
	if profile != nil {
		// A dropped member must not linger as admin (the roster would not verify).
		cp := *profile
		cp.Admins = nil
		for _, a := range profile.Admins {
			if present[a] {
				cp.Admins = append(cp.Admins, a)
			}
		}
		profile = &cp
	}
	next.Profile = profile
	priv, err := id.EdPrivate()
	if err != nil {
		return err
	}
	next.Sign(priv)
	if err := next.Verify(); err != nil {
		return err
	}
	if err := n.groupRelayClient(next.Relay).PublishGroup(ctx, next); err != nil {
		return fmt.Errorf("%w: republishing the roster: %v", ErrNetwork, err)
	}
	st.Roster = next
	if err := n.Groups.Put(st); err != nil {
		return err
	}
	if len(droppedCards) > 0 {
		n.rekeyGroup(ctx, st, "member removal")
	}
	for _, c := range addedCards {
		fp, _ := c.Fingerprint()
		invite := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: fp, TS: time.Now(),
			Type: a2a.TypeGroupInvite, GID: next.GroupID, Group: next, Body: next.Name}
		if err := n.sendGroupPairwise(ctx, c, invite); err != nil {
			n.logf("group %s: invite to %s failed: %v", a2a.ShortFp(next.GroupID), a2a.ShortFp(fp), err)
		}
	}
	if len(addedCards) > 0 && len(droppedCards) == 0 {
		// No rekey happened: top my current key up to the new members.
		n.distributeSenderKey(ctx, st)
	}
	n.groupNotifyUpdate(ctx, st, "roster updated")
	// The fan-out above follows the NEW roster and cannot reach removed members — tell
	// them pairwise so their node refetches, hits 403 and marks the group read-only.
	var removedFps []string
	for _, c := range droppedCards {
		fp, _ := c.Fingerprint()
		removedFps = append(removedFps, fp)
		bye := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: fp, TS: time.Now(),
			Type: a2a.TypeGroupUpdate, GID: next.GroupID}
		if err := n.sendGroupPairwise(ctx, c, bye); err != nil {
			n.logf("group %s: removal notice to %s failed: %v", a2a.ShortFp(next.GroupID), a2a.ShortFp(fp), err)
		}
	}
	var addedFps []string
	for _, c := range addedCards {
		fp, _ := c.Fingerprint()
		addedFps = append(addedFps, fp)
	}
	sort.Strings(addedFps)
	sort.Strings(removedFps)
	n.emit(Event{Kind: EventGroupUpdated, GID: next.GroupID, TS: time.Now(), Reason: GroupReasonRoster,
		Added: addedFps, Removed: removedFps})
	return nil
}

// handleGroupUpdatePairwise: someone (normally the owner, e.g. after removing me) poked
// me to refetch a roster. The refetch itself is the trust anchor (signed roster / 403),
// so the poke needs no further validation beyond holding the group.
func (n *Peer) handleGroupUpdatePairwise(msg *a2a.Message) error {
	if !a2a.ValidGroupID(msg.GID) || n.Groups.Get(msg.GID) == nil {
		return nil
	}
	n.refreshRoster(context.Background(), msg.GID)
	return nil
}

// groupNotifyUpdate fans a group_update through the relay (best effort) so members
// refetch the roster.
func (n *Peer) groupNotifyUpdate(ctx context.Context, st *a2a.GroupState, note string) {
	gid := st.Roster.GroupID
	msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), GID: gid, TS: time.Now(),
		Type: a2a.TypeGroupUpdate, Body: note}
	if err := n.fanOutGroup(ctx, st, msg); err != nil {
		n.logf("group %s: update notice failed: %v", a2a.ShortFp(gid), err)
	}
}

// relayForbidden: the relay refused us by STATUS (403) - the only reliable
// signal that we are no longer a member. Message text is localized per relay
// implementation and must never be matched.
func relayForbidden(err error) bool {
	var re *a2a.RelayError
	return errors.As(err, &re) && re.StatusCode == http.StatusForbidden
}

// refreshRoster refetches the roster from the group relay and applies it: version must
// increase and the owner must be unchanged. Removals trigger a rekey; my own removal
// keeps the group read-only (see GroupLeft).
func (n *Peer) refreshRoster(ctx context.Context, gid string) {
	st := n.Groups.Get(gid)
	if st == nil {
		return
	}
	fetched, err := n.groupRelayClient(st.Roster.Relay).FetchGroup(ctx, gid)
	if err != nil {
		n.logf("group %s: roster refresh failed: %v", a2a.ShortFp(gid), err)
		if relayForbidden(err) && !n.GroupLeft(gid) {
			// The relay no longer counts us as a member: we were removed.
			n.logf("group %s: I was removed (relay 403); keeping the group read-only (archive kept)", a2a.ShortFp(gid))
			n.markGroupLeft(gid, "removed")
			n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonRemoved})
		}
		return
	}
	n.applyRoster(ctx, st, fetched)
}

// applyRoster validates and stores a newer roster version (from a fetch or an invite)
// and converges my key material to it. Returns whether the roster was applied.
func (n *Peer) applyRoster(ctx context.Context, st *a2a.GroupState, next *a2a.GroupRoster) bool {
	gid := st.Roster.GroupID
	if next.GroupID != gid || next.Verify() != nil || next.OwnerPub != st.Roster.OwnerPub {
		n.logf("group %s: rejected a roster update (id/owner/signature mismatch)", a2a.ShortFp(gid))
		return false
	}
	if next.Version <= st.Roster.Version {
		return false
	}
	me := n.Fingerprint()
	if next.Member(me) == nil {
		// I was removed. The group and its archive stay on disk read-only: the newest
		// roster is still the truth about who is in, and keeping keys.json (my sender-key
		// epoch) is what lets a later re-admission rotate PAST it instead of restarting at
		// epoch 1 - which a member that still remembers me would take for a replay.
		st.Roster = next
		if err := n.Groups.Put(st); err != nil {
			n.logf("group %s: storing roster update: %v", a2a.ShortFp(gid), err)
		}
		if !n.GroupLeft(gid) {
			n.logf("group %s: I was removed; keeping the group read-only (archive kept)", a2a.ShortFp(gid))
			n.markGroupLeft(gid, "removed")
		}
		n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonRemoved})
		return true
	}
	if n.GroupLeft(gid) {
		// Re-admitted: the same conversation continues. My sender key must move to a NEW
		// epoch and go out to everyone - a member that still holds my old receive state
		// would take a same-epoch chain for a replay, one that forgot me accepts any
		// epoch; a higher epoch covers both. The diff against the roster I held while
		// out is meaningless, so the add/remove bookkeeping below is skipped.
		st.Roster = next
		st.JoinedAt = time.Now()
		if err := n.Groups.Put(st); err != nil {
			n.logf("group %s: storing roster update: %v", a2a.ShortFp(gid), err)
			return false
		}
		n.clearGroupLeft(gid)
		n.rekeyGroup(ctx, st, "re-admitted")
		n.logf("re-joined group %q (%s), %d members (same conversation)", next.Name, a2a.ShortFp(gid), len(next.Members))
		n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonRejoined})
		n.flushInviteIntents(ctx, st)
		return true
	}
	oldFps := map[string]bool{}
	for _, fp := range st.Roster.MemberFps() {
		oldFps[fp] = true
	}
	newFps := map[string]bool{}
	var added, removed []string
	for _, fp := range next.MemberFps() {
		newFps[fp] = true
		if !oldFps[fp] {
			added = append(added, fp)
		}
	}
	for _, fp := range st.Roster.MemberFps() {
		if !newFps[fp] {
			removed = append(removed, fp)
		}
	}
	st.Roster = next
	if err := n.Groups.Put(st); err != nil {
		n.logf("group %s: storing roster update: %v", a2a.ShortFp(gid), err)
		return false
	}
	if len(removed) > 0 {
		n.rekeyGroup(ctx, st, "member removal")
	} else {
		// New members may have joined: top up my key to them.
		n.distributeSenderKey(ctx, st)
	}
	n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonRoster,
		Added: added, Removed: removed})
	n.flushInviteIntents(ctx, st)
	return true
}

// ——— "removed from this group" marker (groups/<gid>/left.json) ———
//
// Being kicked must not wipe the group: the conversation is the user's data, and keeping
// keys.json is exactly what lets a later re-admission rotate my sender key past the old
// epoch (see applyRoster). The marker is local only - never on the wire, never in the
// roster. A voluntary GroupLeave still removes the whole directory: that is the user's
// own choice; a removal is someone else's action and must not lose their data.

type groupLeftMark struct {
	TS     time.Time `json:"ts"`
	Reason string    `json:"reason,omitempty"` // "removed"
}

func (n *Peer) leftPath(gid string) string {
	return filepath.Join(n.Home, "a2a", "groups", a2a.SanitizeID(gid), "left.json")
}

// GroupLeft reports whether the owner removed me from gid while I still hold the group
// locally (read-only: history intact, GroupSend refused, no new mail). Cleared when the
// owner re-admits me.
func (n *Peer) GroupLeft(gid string) bool {
	_, err := os.Stat(n.leftPath(gid))
	return err == nil
}

// markGroupLeft writes the marker (idempotent; a group directory that does not exist is
// not created for it).
func (n *Peer) markGroupLeft(gid, reason string) {
	path := n.leftPath(gid)
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return
	}
	raw, _ := json.Marshal(groupLeftMark{TS: time.Now(), Reason: reason})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		n.logf("group %s: persisting the removed marker: %v", a2a.ShortFp(gid), err)
	}
}

func (n *Peer) clearGroupLeft(gid string) { _ = os.Remove(n.leftPath(gid)) }

// ——— receive side ———

// retryBudget bounds how often one problem key is retried as "transient" before the
// letter is dropped (poison-mail guard; e.g. a group_key for a group whose invite never
// arrives). Retries run at ≥500ms apart (see the Run loop's transient pause), so 60
// attempts ≈ half a minute and more — far beyond any honest in-flight race.
func (n *Peer) retryBudget(key string) bool {
	n.rbMu.Lock()
	defer n.rbMu.Unlock()
	if len(n.rbSeen) > 1000 {
		n.rbSeen = map[string]int{}
	}
	n.rbSeen[key]++
	return n.rbSeen[key] <= 60
}

func (n *Peer) transientOrDrop(key string, err error) error {
	if n.retryBudget(key) {
		return err
	}
	return permanent(fmt.Errorf("retry budget exhausted: %w", err))
}

// handleGroupEnvelope processes one fan-out letter: verify the sender signature and
// membership (never trust the relay), decrypt with the sender's chain, dedupe, archive.
func (n *Peer) handleGroupEnvelope(env *a2a.Envelope) error {
	senderFp, err := env.VerifyGroupEnvelope()
	if err != nil {
		return permanent(err)
	}
	gid := env.GID
	if senderFp == n.Fingerprint() {
		return nil // echo safety: ack and ignore
	}
	st := n.Groups.Get(gid)
	if st == nil {
		// The invite may still be in flight (fan-out can outrun the pairwise invite).
		n.logf("[grp-debug] envelope gid=%s sender=%s NOT-JOINED (transient retry)", a2a.ShortFp(gid), a2a.ShortFp(senderFp))
		return n.transientOrDrop("grp-env-"+gid, fmt.Errorf("group %s not joined yet", a2a.ShortFp(gid)))
	}
	if n.GroupLeft(gid) {
		// A removed member gets no fan-out from the relay; one that arrives anyway most
		// likely means "just re-admitted, the invite is still in flight" - wait for it
		// like the not-joined case (the retry budget bounds a genuine stray).
		return n.transientOrDrop("grp-env-"+gid, fmt.Errorf("group %s: not a member any more (waiting for a possible re-admission)", a2a.ShortFp(gid)))
	}
	if st.Roster.Member(senderFp) == nil {
		// Maybe my roster is stale; refresh once per budget round, else drop.
		n.logf("[grp-debug] envelope gid=%s sender=%s NOT-IN-MY-ROSTER (refreshing)", a2a.ShortFp(gid), a2a.ShortFp(senderFp))
		n.refreshRoster(context.Background(), gid)
		if st = n.Groups.Get(gid); st == nil || st.Roster.Member(senderFp) == nil {
			return permanent(fmt.Errorf("group %s: sender %s is not a member", a2a.ShortFp(gid), a2a.ShortFp(senderFp)))
		}
	}
	n.gkMu.Lock()
	keys := n.Groups.Keys(gid)
	rst := keys.Senders[senderFp]
	if rst == nil {
		n.gkMu.Unlock()
		n.logf("[grp-debug] envelope gid=%s sender=%s KEY-MISSING (transient retry)", a2a.ShortFp(gid), a2a.ShortFp(senderFp))
		return n.transientOrDrop("grp-key-"+gid+senderFp,
			fmt.Errorf("%w (group %s, sender %s)", a2a.ErrGroupKeyMissing, a2a.ShortFp(gid), a2a.ShortFp(senderFp)))
	}
	msg, err := a2a.GroupOpen(rst, env.Cipher)
	if err != nil {
		n.gkMu.Unlock()
		blobE, blobI := -1, -1
		if raw, derr := base64.StdEncoding.DecodeString(env.Cipher); derr == nil {
			var blob struct {
				E int `json:"e"`
				I int `json:"i"`
			}
			if json.Unmarshal(raw, &blob) == nil {
				blobE, blobI = blob.E, blob.I
			}
		}
		n.logf("[grp-debug] decrypt FAIL gid=%s sender=%s blobE=%d blobI=%d heldEpoch=%d heldIndex=%d err=%v", a2a.ShortFp(gid), a2a.ShortFp(senderFp), blobE, blobI, rst.Epoch, rst.Index, err)
		if errors.Is(err, a2a.ErrGroupKeyMissing) {
			return n.transientOrDrop("grp-epoch-"+gid+senderFp, err)
		}
		return permanent(err)
	}
	err = n.Groups.PutKeys(gid, keys)
	n.gkMu.Unlock()
	if err != nil {
		return err // transient: disk hiccup, retry (the chain state was not persisted)
	}
	if msg.From != senderFp || (msg.GID != "" && msg.GID != gid) {
		return permanent(fmt.Errorf("group inner message From/GID mismatch"))
	}
	if msg.ID == "" {
		return permanent(fmt.Errorf("group message has no id"))
	}
	key := a2a.GroupConvKey(gid)
	if n.Convs.Seen(key, msg.ID) {
		return nil
	}
	switch msg.Type {
	case a2a.TypeTyping:
		// "A member's seat is working here" (Agent = which of their agents). Presence
		// metadata like pairwise typing: never archived, no AllowSpeak.
		n.emit(Event{Kind: EventGroupTyping, GID: gid, Peer: senderFp, TS: time.Now(), Agent: msg.Agent, On: msg.Body != "off"})
		return nil
	case a2a.TypeGroupUpdate:
		n.refreshRoster(context.Background(), gid)
		return nil
	case a2a.TypeGroupPin:
		return n.applyGroupPin(st, senderFp, msg)
	case a2a.TypeGroupVoices:
		// Seat roster metadata: which agent names that member's composer answers to.
		// Not speech (AllowSpeak does not apply), never archived; the update event
		// makes clients refetch the member list.
		n.setGroupVoices(gid, senderFp, msg.Voices)
		n.emit(Event{Kind: EventGroupUpdated, GID: gid, Peer: senderFp, TS: time.Now(), Reason: GroupReasonVoices, Message: msg})
		return nil
	case a2a.TypeText:
		// Receiver-side governance: never trust the sender's own check.
		if err := st.Roster.AllowSpeak(senderFp, msg.By); err != nil {
			n.logf("group %s: dropping post from %s: %v", a2a.ShortFp(gid), a2a.ShortFp(senderFp), err)
			return nil
		}
		stored := *msg
		stored.Body = a2a.StripControlMarkers(stored.Body)
		if stored.Body == "" {
			return nil
		}
		seq, err := n.Convs.AppendSeq(key, &a2a.ConvEntry{Dir: "in", Message: stored})
		if err != nil {
			return err
		}
		n.logf("[grp-debug] RECEIVED text gid=%s sender=%s seq=%d bytes=%d", a2a.ShortFp(gid), a2a.ShortFp(senderFp), seq, len(stored.Body))
		n.emit(Event{Kind: EventGroupMessage, GID: gid, Peer: senderFp, TS: time.Now(), Message: &stored, Seq: seq})
		return nil
	default:
		n.logf("group %s: unknown fan-out type %q (ignored)", a2a.ShortFp(gid), msg.Type)
		return nil
	}
}

// handleGroupInvite (pairwise): the owner sent me a signed roster. Trust rule: the
// inviter must BE the roster owner and be MY FRIEND (strangers cannot drag me into
// groups). Joining = store the roster + distribute my sender key to every member.
func (n *Peer) handleGroupInvite(msg *a2a.Message) error {
	n.logf("[grp-debug] invite received gid=%s from=%s", a2a.ShortFp(msg.GID), a2a.ShortFp(msg.From))
	if msg.Group == nil {
		return permanent(fmt.Errorf("group invite carries no roster"))
	}
	roster := msg.Group
	if err := roster.Verify(); err != nil {
		return permanent(err)
	}
	if msg.GID != roster.GroupID {
		return permanent(fmt.Errorf("invite GID does not match its roster"))
	}
	// The inviter must be empowered by the roster itself: the owner, or an admin the
	// owner signed into the profile (admins forward invites to members they vouch for).
	if roster.OwnerFp() != msg.From && !roster.Profile.IsAdmin(msg.From) {
		return permanent(fmt.Errorf("invite sender is neither the roster owner nor an admin"))
	}
	// Trust rule: strangers cannot drag me into groups. The inviter must be MY FRIEND —
	// or I applied to exactly this group AND the inviter is the owner I applied to. The
	// group id is a public random value not bound to the owner key: whitelisting by gid
	// alone would let a third party self-sign a roster with the same gid and a different
	// owner, drag me into a fake group while my application is pending and collect my
	// sender key. A marker written before the owner was recorded whitelists by gid only
	// (until it expires).
	if !n.Friends.IsFriend(msg.From) {
		owner, applied := n.appliedOwner(roster.GroupID)
		if !applied || (owner != "" && owner != msg.From) {
			n.logf("dropping group invite from %s: neither a friend nor the owner I applied to", a2a.ShortFp(msg.From))
			return nil
		}
	}
	me := n.Fingerprint()
	if roster.Member(me) == nil {
		return permanent(fmt.Errorf("invite roster does not include me"))
	}
	ctx := context.Background()
	if existing := n.Groups.Get(roster.GroupID); existing != nil {
		n.clearApplied(roster.GroupID)
		wasLeft := n.GroupLeft(roster.GroupID)
		n.applyRoster(ctx, existing, roster)
		if !wasLeft && !n.GroupLeft(roster.GroupID) {
			// An invite for a group I already hold means the owner is (re)admitting me -
			// e.g. I was removed and re-added while offline, so no roster diff ever told
			// me, yet the others forgot my key when they rekeyed. Hand my current chain to
			// everyone again; a receiver that still holds it takes it as stated. (A
			// re-admission after a marked removal already rotated and redistributed.)
			if st := n.Groups.Get(roster.GroupID); st != nil {
				n.redistributeSenderKey(ctx, st)
			}
		}
		return nil
	}
	st := &a2a.GroupState{Roster: roster, JoinedAt: time.Now(), LastReadAt: time.Now()}
	if err := n.Groups.Put(st); err != nil {
		return err
	}
	if err := n.ensureSenderKey(roster.GroupID); err != nil {
		return err
	}
	n.distributeSenderKey(ctx, st)
	n.clearApplied(roster.GroupID)
	n.logf("joined group %q (%s), %d members", roster.Name, a2a.ShortFp(roster.GroupID), len(roster.Members))
	n.emit(Event{Kind: EventGroupUpdated, GID: roster.GroupID, Peer: msg.From, TS: time.Now(), Reason: GroupReasonJoined})
	return nil
}

// handleGroupKey (pairwise): a co-member sent me their chain key for one group. The
// chain is stored AT the position the sender declares (GroupKeyDist.Index), never at 0.
func (n *Peer) handleGroupKey(msg *a2a.Message) error {
	if msg.GKey == nil || !a2a.ValidGroupID(msg.GID) {
		return permanent(fmt.Errorf("malformed group_key"))
	}
	chain, err := a2a.DecodeKey(msg.GKey.Chain)
	if err != nil || len(chain) != 32 || msg.GKey.Epoch < 1 || msg.GKey.Index < 0 {
		return permanent(fmt.Errorf("malformed group_key material"))
	}
	st := n.Groups.Get(msg.GID)
	if st == nil {
		return n.transientOrDrop("grp-gk-"+msg.GID+msg.From,
			fmt.Errorf("group_key for a group not joined yet (%s)", a2a.ShortFp(msg.GID)))
	}
	if st.Roster.Member(msg.From) == nil {
		// A new member distributes its key the moment the invite lands, which can outrun
		// the owner's group_update to me: refetch the roster once, then wait (transient)
		// instead of dropping. A genuine non-member exhausts the retry budget.
		n.refreshRoster(context.Background(), msg.GID)
		if st = n.Groups.Get(msg.GID); st == nil || st.Roster.Member(msg.From) == nil {
			return n.transientOrDrop("grp-gk-member-"+msg.GID+msg.From,
				fmt.Errorf("group_key from non-member %s (roster may still be in flight)", a2a.ShortFp(msg.From)))
		}
	}
	n.gkMu.Lock()
	defer n.gkMu.Unlock()
	keys := n.Groups.Keys(msg.GID)
	if existing := keys.Senders[msg.From]; existing != nil {
		if msg.GKey.Epoch < existing.Epoch {
			return nil // superseded by a higher epoch we already hold
		}
		if msg.GKey.Epoch == existing.Epoch && existing.Index == msg.GKey.Index && existing.Chain == msg.GKey.Chain {
			return nil // byte-identical redelivery
		}
		// Same epoch, different material: the sender restarted its chain at this epoch
		// (an older peer that wiped the group when kicked and came back at epoch 1) or
		// re-sent its current position. The distribution is pairwise-encrypted by the
		// sender itself and only decides whether ITS chain decrypts: take it as stated.
		// Moving back to an earlier index merely caches the skipped message keys.
	}
	keys.Senders[msg.From] = &a2a.GroupRecvState{Epoch: msg.GKey.Epoch, Index: msg.GKey.Index, Chain: msg.GKey.Chain}
	return n.Groups.PutKeys(msg.GID, keys)
}

// HandleGroupEnvelope processes one group fan-out envelope (Envelope.GID set) for hosts
// that run their own receive loop instead of Run: signature + membership check, decrypt
// with the sender's chain, dedupe, archive, emit. Classify a non-nil error with
// IsPermanent (ack and drop) versus transient (leave on the relay, retry later).
func (n *Peer) HandleGroupEnvelope(env *a2a.Envelope) error { return n.handleGroupEnvelope(env) }

// HandleGroupMessage dispatches one already-decrypted pairwise group control message
// (group_invite / group_key / group_leave / group_update / group_join / group_admin) for
// hosts that run their own receive loop. handled=false means msg is not a group control
// message and the host keeps it; err classifies with IsPermanent.
func (n *Peer) HandleGroupMessage(msg *a2a.Message) (handled bool, err error) {
	switch msg.Type {
	case a2a.TypeGroupInvite:
		return true, n.handleGroupInvite(msg)
	case a2a.TypeGroupKey:
		return true, n.handleGroupKey(msg)
	case a2a.TypeGroupLeave:
		return true, n.handleGroupLeave(msg)
	case a2a.TypeGroupUpdate:
		return true, n.handleGroupUpdatePairwise(msg)
	case a2a.TypeGroupJoin:
		return true, n.handleGroupJoin(msg)
	case a2a.TypeGroupAdmin:
		return true, n.handleGroupAdmin(msg)
	}
	return false, nil
}

// handleGroupLeave (pairwise, owner side): a member asked out — shrink the roster.
func (n *Peer) handleGroupLeave(msg *a2a.Message) error {
	st := n.Groups.Get(msg.GID)
	if st == nil {
		return nil
	}
	if st.Roster.OwnerFp() != n.Fingerprint() {
		return nil // not mine to administer
	}
	if st.Roster.Member(msg.From) == nil {
		return nil // already gone
	}
	if err := n.republishRoster(context.Background(), st, []string{msg.From}, nil, nil); err != nil {
		return err // transient: retried next round
	}
	n.logf("group %s: %s left (roster now v%d)", a2a.ShortFp(msg.GID), a2a.ShortFp(msg.From), st.Roster.Version)
	return nil
}

// ——— governance: profile, pins, join applications, admins (spec §14.7) ———

// GroupSetProfile replaces the group's governance profile (owner only): the roster is
// republished with the new profile (version+1, re-signed), a group_update is fanned out
// and group.updated is emitted.
func (n *Peer) GroupSetProfile(ctx context.Context, gid string, p *a2a.GroupProfile) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	if st.Roster.OwnerFp() != n.Fingerprint() {
		return fmt.Errorf("%w: only the owner can change the profile", ErrGroupOwner)
	}
	if p == nil {
		return fmt.Errorf("%w: profile must not be empty", ErrBadProfile)
	}
	if err := p.Validate(st.Roster.MemberFps()); err != nil {
		return fmt.Errorf("%w: %v", ErrBadProfile, err)
	}
	return n.republishRoster(ctxOrBackground(ctx), st, nil, nil, p)
}

// GroupPin pins an announcement on the group home (owner/admins only): fans a group_pin
// through the relay and applies it locally. Pins are not part of the chat stream.
func (n *Peer) GroupPin(ctx context.Context, gid, body string) (*a2a.GroupPin, error) {
	if !n.HasIdentity() {
		return nil, ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return nil, ErrNoGroup
	}
	me := n.Fingerprint()
	if !st.Roster.CanAdmin(me) {
		return nil, fmt.Errorf("%w: only the owner and admins can pin", ErrGroupOwner)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("%w: pin body must not be empty", ErrBadFile)
	}
	ctx = ctxOrBackground(ctx)
	msg := &a2a.Message{ID: n.newMsgID(), From: me, GID: gid, TS: time.Now(),
		Type: a2a.TypeGroupPin, Body: body}
	if err := n.fanOutGroup(ctx, st, msg); err != nil {
		n.logf("group %s: pin fan-out failed: %v", a2a.ShortFp(gid), err)
	}
	pin := a2a.GroupPin{ID: msg.ID, From: me, TS: msg.TS, Body: body}
	if err := n.Groups.AddPin(gid, pin); err != nil {
		return nil, err
	}
	n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonPins})
	return &pin, nil
}

// GroupUnpin removes one pinned announcement (owner/admins only) and fans the removal.
func (n *Peer) GroupUnpin(ctx context.Context, gid, pinID string) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	me := n.Fingerprint()
	if !st.Roster.CanAdmin(me) {
		return fmt.Errorf("%w: only the owner and admins can unpin", ErrGroupOwner)
	}
	if strings.TrimSpace(pinID) == "" {
		return fmt.Errorf("%w: pin id must not be empty", ErrBadFile)
	}
	ctx = ctxOrBackground(ctx)
	msg := &a2a.Message{ID: n.newMsgID(), From: me, GID: gid, TS: time.Now(),
		Type: a2a.TypeGroupPin, PinRemove: pinID}
	if err := n.fanOutGroup(ctx, st, msg); err != nil {
		n.logf("group %s: unpin fan-out failed: %v", a2a.ShortFp(gid), err)
	}
	if err := n.Groups.RemovePin(gid, pinID); err != nil {
		return err
	}
	n.emit(Event{Kind: EventGroupUpdated, GID: gid, TS: time.Now(), Reason: GroupReasonPins})
	return nil
}

func pinByID(pins []a2a.GroupPin, id string) *a2a.GroupPin {
	for i := range pins {
		if pins[i].ID == id {
			return &pins[i]
		}
	}
	return nil
}

// applyGroupPin applies one fan-out pin/unpin: only the owner and admins may pin;
// changes are idempotent, never archived to the chat stream, and every actual change
// emits group.updated so clients refetch the pin list.
func (n *Peer) applyGroupPin(st *a2a.GroupState, senderFp string, msg *a2a.Message) error {
	gid := st.Roster.GroupID
	if !st.Roster.CanAdmin(senderFp) {
		n.logf("group %s: dropping pin change from non-admin %s", a2a.ShortFp(gid), a2a.ShortFp(senderFp))
		return nil
	}
	changed := false
	if msg.PinRemove != "" {
		if pinByID(n.Groups.Pins(gid), msg.PinRemove) != nil {
			if err := n.Groups.RemovePin(gid, msg.PinRemove); err != nil {
				return err
			}
			changed = true
		}
	} else {
		body := strings.TrimSpace(msg.Body)
		if body != "" && pinByID(n.Groups.Pins(gid), msg.ID) == nil {
			if err := n.Groups.AddPin(gid, a2a.GroupPin{ID: msg.ID, From: senderFp, TS: msg.TS, Body: body}); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		n.emit(Event{Kind: EventGroupUpdated, GID: gid, Peer: senderFp, TS: time.Now(), Reason: GroupReasonPins})
	}
	return nil
}

// GroupApply applies to join a group by its public handle (soulmirror://group?...):
// fetch the public card from the group's home relay, then send a pairwise group_join
// application to the owner. What happens next is the owner's join policy: open groups
// add mechanically, apply groups pend for approval, invite-only groups drop it.
// GroupLookup fetches the PUBLIC card of a group (join policy, paid-join price
// and receiving address) so a stranger can decide to apply and pay.
func (n *Peer) GroupLookup(ctx context.Context, groupURI string) (*a2a.GroupCard, error) {
	gid, relayURL, _, err := a2a.ParseGroupURI(groupURI)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	ctx = ctxOrBackground(ctx)
	card, err := n.groupRelayClient(relayURL).FetchGroupCard(ctx, gid)
	if err != nil {
		return nil, fmt.Errorf("%w: fetching the group card: %v", ErrNetwork, err)
	}
	if card.OwnerCard == nil || card.OwnerCard.Verify() != nil {
		return nil, fmt.Errorf("%w: owner card", ErrBadCard)
	}
	return card, nil
}

func (n *Peer) GroupApply(ctx context.Context, groupURI, note string, payment *a2a.JoinPayment) (string, error) {
	if !n.HasIdentity() {
		return "", ErrNoIdentity
	}
	gid, relayURL, _, err := a2a.ParseGroupURI(groupURI)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	if n.Groups.Get(gid) != nil {
		return gid, nil // already a member
	}
	ctx = ctxOrBackground(ctx)
	card, err := n.groupRelayClient(relayURL).FetchGroupCard(ctx, gid)
	if err != nil {
		return "", fmt.Errorf("%w: fetching the group card: %v", ErrNetwork, err)
	}
	if err := card.OwnerCard.Verify(); err != nil {
		return "", fmt.Errorf("%w: owner card: %v", ErrBadCard, err)
	}
	ownerFp, err := card.OwnerCard.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	myCard, err := n.Identity().Card()
	if err != nil {
		return "", err
	}
	msg := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: ownerFp, TS: time.Now(),
		Type: a2a.TypeGroupJoin, GID: gid, Body: strings.TrimSpace(note), Card: myCard, Payment: payment}
	n.markApplied(gid, ownerFp) // whitelist the invite THIS owner answers with (see handleGroupInvite)
	if err := n.sendGroupPairwise(ctx, card.OwnerCard, msg); err != nil {
		return "", err
	}
	n.logf("applied to group %q (%s) via %s", card.Name, a2a.ShortFp(gid), relayURL)
	return gid, nil
}

// handleGroupJoin (pairwise, owner side): a stranger applied to join. The card must
// verify and match the sender; the profile's join policy decides mechanically:
// open → add; apply → pend + notify the host; invite (or no profile) → drop.
func (n *Peer) handleGroupJoin(msg *a2a.Message) error {
	if msg.Card == nil {
		return permanent(fmt.Errorf("group_join carries no card"))
	}
	if err := msg.Card.Verify(); err != nil {
		return permanent(fmt.Errorf("group_join card verification failed: %v", err))
	}
	fp, err := msg.Card.Fingerprint()
	if err != nil || fp != msg.From {
		return permanent(fmt.Errorf("group_join card does not match the sender"))
	}
	st := n.Groups.Get(msg.GID)
	if st == nil || st.Roster.OwnerFp() != n.Fingerprint() {
		return nil // not my group to administer
	}
	gid := msg.GID
	if st.Roster.Member(fp) != nil {
		// Already a member: their invite may have been lost — resend it (idempotent).
		invite := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: fp, TS: time.Now(),
			Type: a2a.TypeGroupInvite, GID: gid, Group: st.Roster, Body: st.Roster.Name}
		return n.sendGroupPairwise(context.Background(), msg.Card, invite)
	}
	policy := a2a.JoinInvite
	if st.Roster.Profile != nil && st.Roster.Profile.Join != "" {
		policy = st.Roster.Profile.Join
	}
	switch policy {
	case a2a.JoinOpen:
		if err := n.republishRoster(context.Background(), st, nil, []*a2a.Card{msg.Card}, nil); err != nil {
			return err // transient: retried next round
		}
		n.logf("group %s: open join — added %s", a2a.ShortFp(gid), a2a.ShortFp(fp))
		return nil
	case a2a.JoinApply, a2a.JoinPaid:
		app := &a2a.GroupApplication{Card: msg.Card, Note: msg.Body, TS: time.Now()}
		// Keep any attached payment proof for the owner to verify — both native
		// paid groups and compat-encoded ones (join=apply + #paid-join marker in
		// rules on old relays) arrive here with msg.Payment set.
		if msg.Payment != nil {
			app.Payment = msg.Payment
		}
		if policy == a2a.JoinPaid && msg.Payment == nil {
			n.logf("group %s: paid join application from %s has no payment proof — pended for the owner", a2a.ShortFp(gid), a2a.ShortFp(fp))
		}
		if err := n.Groups.PutApplication(gid, app); err != nil {
			return err
		}
		n.logf("group %s: join application from %s pended (policy=%s)", a2a.ShortFp(gid), a2a.ShortFp(fp), policy)
		n.emit(Event{Kind: EventGroupApplication, GID: gid, Peer: fp, TS: time.Now(), Message: msg})
		return nil
	default: // invite-only: applications are not accepted
		n.logf("group %s: dropping join application from %s (invite-only)", a2a.ShortFp(gid), a2a.ShortFp(fp))
		return nil
	}
}

func (n *Peer) groupApplicationViews(gid string) []GroupApplicationView {
	apps := n.Groups.Applications(gid)
	out := make([]GroupApplicationView, 0, len(apps))
	for _, app := range apps {
		fp, err := app.Card.Fingerprint()
		if err != nil {
			continue
		}
		out = append(out, GroupApplicationView{Fp: fp, Name: cardName(app.Card), Note: app.Note, TS: app.TS, Payment: app.Payment})
	}
	return out
}

// GroupApplications lists the pending join applications of one group. Applications only
// exist on the owner's node; elsewhere the list is empty.
func (n *Peer) GroupApplications(gid string) ([]GroupApplicationView, error) {
	if n.Groups.Get(gid) == nil {
		return nil, ErrNoGroup
	}
	return n.groupApplicationViews(gid), nil
}

// GroupApprove accepts one pending application (owner only — applications live on the
// owner's node): the applicant joins the roster (republish + invite + keys) and the
// application is removed.
func (n *Peer) GroupApprove(ctx context.Context, gid, fp string) error {
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	if st.Roster.OwnerFp() != n.Fingerprint() {
		return fmt.Errorf("%w: only the owner approves applications", ErrGroupOwner)
	}
	var app *a2a.GroupApplication
	for _, a := range n.Groups.Applications(gid) {
		if f, err := a.Card.Fingerprint(); err == nil && f == fp {
			app = a
			break
		}
	}
	if app == nil {
		return fmt.Errorf("%w: no join application from %s", ErrNoPending, a2a.ShortFp(fp))
	}
	if app.Payment != nil && app.Payment.TxHash != "" {
		// Replay guard (paid groups): claim the payment tx atomically BEFORE
		// admitting — a single on-chain transfer admits exactly one member, so
		// reusing a tx_hash (by anyone, for any later application) is refused.
		claimed, err := n.Groups.ConsumePaymentTx(gid, app.Payment.TxHash)
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("%w: tx %s has already been used to join this group", ErrPaidProofUsed, a2a.ShortFp(app.Payment.TxHash))
		}
	}
	if st.Roster.Member(fp) == nil {
		if err := n.republishRoster(ctxOrBackground(ctx), st, nil, []*a2a.Card{app.Card}, nil); err != nil {
			return err
		}
	}
	return n.Groups.RemoveApplication(gid, fp)
}

// GroupRejectApplication discards one pending application (owner only; no notice sent).
func (n *Peer) GroupRejectApplication(gid, fp string) error {
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	if st.Roster.OwnerFp() != n.Fingerprint() {
		return fmt.Errorf("%w: only the owner rejects applications", ErrGroupOwner)
	}
	return n.Groups.RemoveApplication(gid, fp)
}

// GroupInvite adds a FRIEND of mine to the group. The owner republishes directly; an
// admin (per the roster profile) forwards a group_admin request to the owner, whose
// node executes it mechanically — the admin then passes the invite on once the roster
// includes the friend (see flushInviteIntents); anyone else is refused.
func (n *Peer) GroupInvite(ctx context.Context, gid, friendFp string) error {
	if !n.HasIdentity() {
		return ErrNoIdentity
	}
	st := n.Groups.Get(gid)
	if st == nil {
		return ErrNoGroup
	}
	fr := n.Friends.Get(friendFp)
	if fr == nil || fr.Card == nil {
		return fmt.Errorf("%w: %s", ErrNotFriend, a2a.ShortFp(friendFp))
	}
	if st.Roster.Member(friendFp) != nil {
		return nil // already a member
	}
	me := n.Fingerprint()
	ctx = ctxOrBackground(ctx)
	if st.Roster.OwnerFp() == me {
		return n.republishRoster(ctx, st, nil, []*a2a.Card{fr.Card}, nil)
	}
	if st.Roster.Profile.IsAdmin(me) {
		ownerCard := st.Roster.Member(st.Roster.OwnerFp())
		if ownerCard == nil {
			return fmt.Errorf("roster has no owner card")
		}
		n.recordInviteIntent(gid, friendFp) // send the invite myself once the owner adds them
		req := &a2a.Message{ID: n.newMsgID(), From: me, To: st.Roster.OwnerFp(), TS: time.Now(),
			Type: a2a.TypeGroupAdmin, GID: gid, Body: "invite", Card: fr.Card}
		return n.sendGroupPairwise(ctx, ownerCard, req)
	}
	return fmt.Errorf("%w: only the owner or an admin can invite", ErrGroupOwner)
}

// handleGroupAdmin (pairwise, owner side): an admin (per the CURRENT roster profile)
// asked for an invite or a kick; execute mechanically and idempotently.
func (n *Peer) handleGroupAdmin(msg *a2a.Message) error {
	st := n.Groups.Get(msg.GID)
	if st == nil || st.Roster.OwnerFp() != n.Fingerprint() {
		return nil // not my group to administer
	}
	gid := msg.GID
	if !st.Roster.Profile.IsAdmin(msg.From) {
		n.logf("group %s: dropping admin request from non-admin %s", a2a.ShortFp(gid), a2a.ShortFp(msg.From))
		return nil
	}
	ctx := context.Background()
	body := strings.TrimSpace(msg.Body)
	switch {
	case body == "invite":
		if msg.Card == nil {
			return permanent(fmt.Errorf("admin invite carries no card"))
		}
		if err := msg.Card.Verify(); err != nil {
			return permanent(fmt.Errorf("admin invite card verification failed: %v", err))
		}
		fp, err := msg.Card.Fingerprint()
		if err != nil {
			return permanent(err)
		}
		if st.Roster.Member(fp) != nil {
			return nil // already a member
		}
		if err := n.republishRoster(ctx, st, nil, []*a2a.Card{msg.Card}, nil); err != nil {
			return err // transient: retried next round
		}
		n.logf("group %s: admin %s invited %s", a2a.ShortFp(gid), a2a.ShortFp(msg.From), a2a.ShortFp(fp))
		return nil
	case strings.HasPrefix(body, "kick "):
		fp := strings.TrimSpace(strings.TrimPrefix(body, "kick "))
		if fp == "" || fp == st.Roster.OwnerFp() {
			n.logf("group %s: refusing admin kick of the owner", a2a.ShortFp(gid))
			return nil
		}
		if st.Roster.Member(fp) == nil {
			return nil // already gone
		}
		if err := n.republishRoster(ctx, st, []string{fp}, nil, nil); err != nil {
			return err
		}
		n.logf("group %s: admin %s kicked %s", a2a.ShortFp(gid), a2a.ShortFp(msg.From), a2a.ShortFp(fp))
		return nil
	default:
		n.logf("group %s: unknown admin request %q (ignored)", a2a.ShortFp(gid), body)
		return nil
	}
}

// ——— small persisted markers (applicant side + admin side) ———

// appliedPath marks "I applied to this group": the marker whitelists the group invite
// that will come back from the owner — a stranger — so it is not dropped. It records the
// owner I applied to (the whitelist is bound to that owner, see handleGroupInvite) and
// expires: an application that is never answered must not become a standing "accept any
// invite for this gid".
func (n *Peer) appliedPath(gid string) string {
	return filepath.Join(n.Home, "a2a", "groups", a2a.SanitizeID(gid), "applied.json")
}

// appliedTTL bounds how long a pending application whitelists the owner's invite.
const appliedTTL = 30 * 24 * time.Hour

type appliedMark struct {
	Owner string    `json:"owner,omitempty"` // fingerprint of the owner I applied to ("" on markers written before it was recorded)
	TS    time.Time `json:"ts"`
}

func (n *Peer) markApplied(gid, ownerFp string) {
	path := n.appliedPath(gid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		n.logf("group %s: persisting application marker: %v", a2a.ShortFp(gid), err)
		return
	}
	raw, _ := json.Marshal(appliedMark{Owner: ownerFp, TS: time.Now()})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		n.logf("group %s: persisting application marker: %v", a2a.ShortFp(gid), err)
	}
}

// appliedOwner returns the owner recorded with my pending application to gid ("" for a
// marker that predates the owner field) and whether a live marker exists. Expired
// markers are removed.
func (n *Peer) appliedOwner(gid string) (string, bool) {
	raw, err := os.ReadFile(n.appliedPath(gid))
	if err != nil {
		return "", false
	}
	var m appliedMark
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	if !m.TS.IsZero() && time.Since(m.TS) > appliedTTL {
		n.clearApplied(gid)
		return "", false
	}
	return m.Owner, true
}

// GroupApplied reports whether I applied to gid and the application is still pending
// (no invite has come back yet). Hosts render "applied, waiting for the owner" from it.
func (n *Peer) GroupApplied(gid string) bool {
	_, ok := n.appliedOwner(gid)
	return ok
}

func (n *Peer) clearApplied(gid string) { _ = os.Remove(n.appliedPath(gid)) }

// intentsPath holds the friends I (an admin) asked the owner to add: once the roster
// includes them, I forward them the invite (their trust check needs a FRIEND's invite).
func (n *Peer) intentsPath(gid string) string {
	return filepath.Join(n.Home, "a2a", "groups", a2a.SanitizeID(gid), "invite-intents.json")
}

func (n *Peer) readInviteIntents(gid string) []string {
	raw, err := os.ReadFile(n.intentsPath(gid))
	if err != nil {
		return nil
	}
	var fps []string
	if json.Unmarshal(raw, &fps) != nil {
		return nil
	}
	return fps
}

func (n *Peer) writeInviteIntents(gid string, fps []string) {
	path := n.intentsPath(gid)
	if len(fps) == 0 {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		n.logf("group %s: persisting invite intents: %v", a2a.ShortFp(gid), err)
		return
	}
	raw, _ := json.Marshal(fps)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		n.logf("group %s: persisting invite intents: %v", a2a.ShortFp(gid), err)
	}
}

func (n *Peer) recordInviteIntent(gid, fp string) {
	fps := n.readInviteIntents(gid)
	for _, f := range fps {
		if f == fp {
			return
		}
	}
	n.writeInviteIntents(gid, append(fps, fp))
}

// flushInviteIntents sends MY invite to friends I asked the owner to add, once the
// roster actually includes them. Called after every applied roster update.
func (n *Peer) flushInviteIntents(ctx context.Context, st *a2a.GroupState) {
	gid := st.Roster.GroupID
	intents := n.readInviteIntents(gid)
	if len(intents) == 0 {
		return
	}
	var remaining []string
	for _, fp := range intents {
		card := st.Roster.Member(fp)
		if card == nil {
			remaining = append(remaining, fp) // not added yet: keep waiting
			continue
		}
		invite := &a2a.Message{ID: n.newMsgID(), From: n.Fingerprint(), To: fp, TS: time.Now(),
			Type: a2a.TypeGroupInvite, GID: gid, Group: st.Roster, Body: st.Roster.Name}
		if err := n.sendGroupPairwise(ctx, card, invite); err != nil {
			n.logf("group %s: forwarding invite to %s failed: %v", a2a.ShortFp(gid), a2a.ShortFp(fp), err)
			remaining = append(remaining, fp)
		}
	}
	n.writeInviteIntents(gid, remaining)
}
