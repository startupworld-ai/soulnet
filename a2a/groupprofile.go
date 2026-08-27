// GroupProfile (wire spec §14.7): the group's constitution, signed as part of the
// roster. Three layers make a "dynamic group":
//
//	transport  — sender-key fan-out (§14.1–14.6), payload-agnostic
//	governance — THIS: a few machine-enforced switches + free-text rules for agents
//	room       — the pluggable application rendering the group (profile.Room; "chat"
//	             is the built-in default room, other rooms mount through the same
//	             client-side room-module interface)
//
// Design rule (minimal concepts): only what machines must ENFORCE becomes a structured
// switch; everything about how agents should behave goes into Rules as free text that
// is injected into the agent's prompt alongside the diplomacy protocol.
package a2a

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Message provenance (Message.By): who produced a group message. Self-declared, like
// `auto`; enforced by sending AND receiving members against the group profile.
const (
	ByOwner = "owner" // the human typed it
	ByAlter = "alter" // the alter composed it
)

// Group message/control types on top of the §14.5 set.
const (
	TypeGroupPin   = "group_pin"   // fan-out, owner/admin: pin (body=text) or unpin (PinRemove=pin id); lands on the group home, not the chat stream
	TypeGroupJoin  = "group_join"  // pairwise, stranger → owner: application to join (body=note, Card=applicant card)
	TypeGroupAdmin = "group_admin" // pairwise, admin → owner: body="invite" (Card=invitee) or "kick <fp>"; the owner's node executes mechanically
)

// GroupProfile field bounds.
const (
	MaxGroupRulesLen = 16 << 10 // free-text rules cap (bytes)
	MaxGroupTags     = 8
	MaxGroupTagLen   = 32
	MaxGroupAdmins   = 16
)

// JoinPayment is the paid-join proof an applicant attaches to a group_join
// (Message.Payment): the on-chain USDC transfer they made to the group's
// JoinAddr. Verified by the owner's node against the public chain before
// approving (amount ≥ join_price, recipient == join_addr). Replay protection:
// the owner consumes each tx_hash once (one tx admits one member) and — when
// Payer/Proof are present — requires the on-chain sender to be an address the
// applicant proved control of (a wallet-secret receipt, see join.receipt).
type JoinPayment struct {
	TxHash string `json:"tx_hash"` // 0x transaction hash (Base)
	Amount string `json:"amount"`  // decimal USDC, e.g. "1.00"
	To     string `json:"to"`      // 0x recipient address (must equal join_addr)
	// Payer is the 0x address the applicant paid from (identity binding: the
	// owner rejects proofs whose on-chain sender differs).
	Payer string `json:"payer,omitempty"`
	// Proof is the wallet-secret receipt (minted by the applicant's local
	// paygate) proving the applicant controls Payer. Optional — without it the
	// owner still enforces sender==Payer, but cannot cryptographically tie the
	// wallet to the applicant.
	Proof *JoinPaymentProof `json:"proof,omitempty"`
	Note  string            `json:"note,omitempty"` // optional display note ("paid 1 USDC to join")
}

// JoinPaymentProof is the wire form of the wallet-secret receipt minted by
// the applicant's paygate (POST /v2/pay/join.receipt).
type JoinPaymentProof struct {
	Message string `json:"message"` // canonical JSON {"fp","tx_hash","payer"}
	Pubkey  string `json:"pubkey"`  // hex 0x04||x||y (uncompressed P-256 public key)
	Sig     string `json:"sig"`     // hex R||S (raw ES256 signature)
}

// Speak scopes (GroupProfile.SpeakWho).
const (
	SpeakAll    = "all"
	SpeakOwner  = "owner"
	SpeakAdmins = "admins"
)

// Join policies (GroupProfile.Join).
const (
	JoinInvite = "invite" // members enter by owner/admin invitation only
	JoinApply  = "apply"  // strangers may apply (group_join); owner approves
	JoinOpen   = "open"   // strangers who apply are added mechanically
	// JoinPaid: strangers may apply only after paying join_price USDC to
	// join_addr (a public on-chain address); the application carries a Payment
	// proof that the owner's node verifies (public RPC) before approving.
	JoinPaid = "paid"
)

// Agent wake policies (GroupProfile.AgentWake).
const (
	WakeMention = "mention" // the alter wakes only when named (@name) or on owner instruction
	WakeAlways  = "always"
	WakeNever   = "never"
)

// GroupProfile is the governance layer of one group. A nil profile (pre-§14.7 groups)
// means "everything allowed, chat room, invite-only".
type GroupProfile struct {
	// Template names the preset this profile started from (display only).
	Template string `json:"template,omitempty"`
	// Room is the room application rendering this group. "" or "chat" = the built-in
	// chat room; other values are room-module ids (custom room bundles come later).
	Room string `json:"room,omitempty"`
	// SpeakHumans / SpeakAgents: may humans (by=owner) / alters (by=alter) post.
	SpeakHumans bool `json:"speak_humans"`
	SpeakAgents bool `json:"speak_agents"`
	// SpeakWho: which MEMBERS may post at all: all | owner | admins ("" = all).
	SpeakWho string `json:"speak_who,omitempty"`
	// Join: invite | apply | open | paid ("" = invite).
	Join string `json:"join,omitempty"`
	// JoinPrice is the paid-join price in USDC (decimal string, e.g. "1.00");
	// required when Join == "paid".
	JoinPrice string `json:"join_price,omitempty"`
	// JoinAddr is the paid-join receiving address (0x, Base); required when
	// Join == "paid". Defaults to the owner's published wallet address.
	JoinAddr string `json:"join_addr,omitempty"`
	// JoinNote is the payment instruction shown to applicants (amount, address,
	// what to write in the application). Display only.
	JoinNote string `json:"join_note,omitempty"`
	// AgentWake: when a member's alter wakes on group traffic: mention | always | never ("" = mention).
	AgentWake string `json:"agent_wake,omitempty"`
	// AgentTier: default reply tier of alters in this group: notify | draft | auto ("" = draft).
	AgentTier string `json:"agent_tier,omitempty"`
	// AutoPerHour caps one alter's automatic posts per hour (0 = default 10).
	AutoPerHour int `json:"auto_per_hour,omitempty"`
	// AgentRounds caps consecutive alter-only exchanges before agents go quiet until a
	// human (or a mention) speaks (0 = default 3).
	AgentRounds int `json:"agent_rounds,omitempty"`
	// Admins: member fingerprints with invite/kick/pin rights (the owner always has them).
	Admins []string `json:"admins,omitempty"`
	// Public lists the group's card on its relay (GET /group/card, /group/search) so
	// strangers can find it and apply. Meaningful with Join apply/open.
	Public bool     `json:"public,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	// Rules is the free-text group constitution (markdown): how agents should behave
	// here, what the group is about. Injected into member alters' prompts.
	Rules string `json:"rules,omitempty"`
}

// DefaultGroupProfile is the "standard group" template: humans and agents both speak,
// agents wake on mention and draft their replies, invite-only, chat room.
func DefaultGroupProfile() *GroupProfile {
	return &GroupProfile{Template: "standard", Room: "chat", SpeakHumans: true, SpeakAgents: true,
		SpeakWho: SpeakAll, Join: JoinInvite, AgentWake: WakeMention, AgentTier: "draft",
		AutoPerHour: 10, AgentRounds: 3}
}

func oneOf(v string, allowed ...string) bool {
	if v == "" {
		return true
	}
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// validUSDCAmount accepts a positive USDC decimal string like "1.00" / "0.5" / "10"
// (no exponent, at most 6 fraction digits, strictly positive).
func validUSDCAmount(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	parts := strings.SplitN(s, ".", 2)
	if parts[0] == "" {
		return false
	}
	for _, ch := range parts[0] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	if len(parts) == 2 {
		if parts[1] == "" || len(parts[1]) > 6 {
			return false
		}
		for _, ch := range parts[1] {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	// strictly positive: "0" / "0.000000" are not a price
	nz := false
	for _, ch := range s {
		if ch >= '1' && ch <= '9' {
			nz = true
			break
		}
	}
	return nz
}

// validHexAddress accepts a 0x-prefixed 40-hex-char EVM address.
func validHexAddress(s string) bool {
	s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if len(s) != 40 {
		return false
	}
	for _, ch := range s {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

// Validate checks the profile's shape. memberFps (when non-nil) additionally pins every
// admin to a current member.
func (p *GroupProfile) Validate(memberFps []string) error {
	if p == nil {
		return nil
	}
	if !oneOf(p.SpeakWho, SpeakAll, SpeakOwner, SpeakAdmins) {
		return fmt.Errorf("speak_who must be all|owner|admins")
	}
	if !oneOf(p.Join, JoinInvite, JoinApply, JoinOpen, JoinPaid) {
		return fmt.Errorf("join must be invite|apply|open|paid")
	}
	if p.Join == JoinPaid {
		if p.JoinPrice == "" {
			return fmt.Errorf("join=paid requires join_price (USDC decimal, e.g. \"1.00\")")
		}
		if !validUSDCAmount(p.JoinPrice) {
			return fmt.Errorf("join_price must be a positive USDC decimal, e.g. \"1.00\"")
		}
		if !validHexAddress(p.JoinAddr) {
			return fmt.Errorf("join=paid requires join_addr (0x Base address)")
		}
	}
	if !oneOf(p.AgentWake, WakeMention, WakeAlways, WakeNever) {
		return fmt.Errorf("agent_wake must be mention|always|never")
	}
	if !oneOf(p.AgentTier, "notify", "draft", "auto") {
		return fmt.Errorf("agent_tier must be notify|draft|auto")
	}
	if !p.SpeakHumans && !p.SpeakAgents {
		return fmt.Errorf("at least one of speak_humans/speak_agents must be true")
	}
	if p.AutoPerHour < 0 || p.AgentRounds < 0 {
		return fmt.Errorf("caps must not be negative")
	}
	if len(p.Rules) > MaxGroupRulesLen {
		return fmt.Errorf("rules exceed %d bytes", MaxGroupRulesLen)
	}
	if len(p.Admins) > MaxGroupAdmins {
		return fmt.Errorf("too many admins")
	}
	if len(p.Tags) > MaxGroupTags {
		return fmt.Errorf("too many tags")
	}
	for _, t := range p.Tags {
		if t == "" || len([]rune(t)) > MaxGroupTagLen {
			return fmt.Errorf("tags must be 1-%d characters", MaxGroupTagLen)
		}
	}
	if memberFps != nil {
		in := map[string]bool{}
		for _, fp := range memberFps {
			in[fp] = true
		}
		for _, a := range p.Admins {
			if !in[a] {
				return fmt.Errorf("admin %s is not a member", ShortFp(a))
			}
		}
	}
	return nil
}

// canonical is the profile's deterministic serialization inside the roster signing
// string (Go encoding/json with a fixed struct: declaration-order fields; non-Go
// implementations must replicate the exact bytes — same caveat as the §8 Profile).
func (p *GroupProfile) canonical() string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// IsAdmin reports whether fp is listed as an admin (the OWNER is not implied here —
// callers check the owner separately).
func (p *GroupProfile) IsAdmin(fp string) bool {
	if p == nil {
		return false
	}
	for _, a := range p.Admins {
		if a == fp {
			return true
		}
	}
	return false
}

// CanAdmin reports whether fp may invite/kick/pin in this group: the owner always, plus
// listed admins.
func (g *GroupRoster) CanAdmin(fp string) bool {
	return fp == g.OwnerFp() || g.Profile.IsAdmin(fp)
}

// AllowSpeak checks the governance switches for one post: is member fp allowed to post
// with provenance by ("" counts as owner/human). Both the SENDING node (refuse to send)
// and every RECEIVING node (drop violations) call this — the relay cannot, it only sees
// ciphertext. A nil profile allows everything (legacy groups).
func (g *GroupRoster) AllowSpeak(fp, by string) error {
	p := g.Profile
	if p == nil {
		return nil
	}
	who := p.SpeakWho
	if who == "" {
		who = SpeakAll
	}
	switch who {
	case SpeakOwner:
		if fp != g.OwnerFp() {
			return fmt.Errorf("only the owner speaks in this group")
		}
	case SpeakAdmins:
		if !g.CanAdmin(fp) {
			return fmt.Errorf("only the owner and admins speak in this group")
		}
	}
	if by == ByAlter {
		if !p.SpeakAgents {
			return fmt.Errorf("agents do not speak in this group")
		}
	} else {
		if !p.SpeakHumans {
			return fmt.Errorf("humans do not speak in this group (the alter does)")
		}
	}
	return nil
}

// ——— Group card URI (the join handle a stranger pastes) ———

// EncodeGroupURI encodes the public join handle of a group: gid + home relay (+ name for
// display). The stranger's node fetches the public card from that relay and applies.
func EncodeGroupURI(gid, relay, name string) string {
	q := url.Values{}
	q.Set("gid", gid)
	q.Set("relay", relay)
	if name != "" {
		q.Set("name", name)
	}
	return "soulmirror://group?" + q.Encode()
}

// ParseGroupURI parses a soulmirror://group?... handle.
func ParseGroupURI(uri string) (gid, relay, name string, err error) {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, "soulmirror://group?") {
		return "", "", "", fmt.Errorf("not a group link")
	}
	q, err := url.ParseQuery(strings.TrimPrefix(uri, "soulmirror://group?"))
	if err != nil {
		return "", "", "", err
	}
	gid, relay, name = q.Get("gid"), q.Get("relay"), q.Get("name")
	if !ValidGroupID(gid) || strings.TrimSpace(relay) == "" {
		return "", "", "", fmt.Errorf("group link needs gid and relay")
	}
	return gid, relay, name, nil
}

// GroupCard is the PUBLIC face of a group (relay GET /group/card, /group/search):
// enough to decide whether to apply, nothing private beyond what the owner opted into.
type GroupCard struct {
	GID       string   `json:"gid"`
	Name      string   `json:"name"`
	Room      string   `json:"room,omitempty"`
	Join      string   `json:"join,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Members   int      `json:"members"`
	RulesHead string   `json:"rules_head,omitempty"` // first part of the rules, for display
	OwnerCard *Card    `json:"owner_card"`           // where applications go (pairwise group_join)
	// Paid-join pricing, published so applicants know what to pay and where
	// (empty unless Join == "paid").
	JoinPrice string `json:"join_price,omitempty"` // USDC decimal, e.g. "1.00"
	JoinAddr  string `json:"join_addr,omitempty"`  // 0x receiving address (Base)
	JoinNote  string `json:"join_note,omitempty"`  // payment instruction (display)
}

// PublicCard derives the public card from a roster, or nil when the group is not public.
func (g *GroupRoster) PublicCard() *GroupCard {
	p := g.Profile
	if p == nil || !p.Public {
		return nil
	}
	head := p.Rules
	if runes := []rune(head); len(runes) > 280 {
		head = string(runes[:280]) + "…"
	}
	join := p.Join
	if join == "" {
		join = JoinInvite
	}
	card := &GroupCard{GID: g.GroupID, Name: g.Name, Room: p.Room, Join: join, Tags: p.Tags,
		Members: len(g.Members), RulesHead: head, OwnerCard: g.Member(g.OwnerFp())}
	if join == JoinPaid {
		card.JoinPrice = p.JoinPrice
		card.JoinAddr = p.JoinAddr
		card.JoinNote = p.JoinNote
	}
	return card
}
