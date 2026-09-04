// Package peer is the soulnet "light peer": a minimal A2A endpoint with no LLM, no wiki
// and no collectors.
//
// It composes the a2a/ protocol layer (identity / card / envelope / relay client /
// friends / conversations / chunking / directory client) into one object that a host
// (a DeepSeek Harness plugin, a script, a system service) can drive:
//
//   - The on-disk layout is identical to the SoulMirror daemon's (<home>/a2a/identity.json ·
//     friends.yaml · profiles/<fp>.json · profile.json · protocol.md ·
//     conversations/<fp>/messages.jsonl · artifacts/<fp>/… · pending/ · outbox/), so moving
//     to SoulMirror is just copying the directory.
//   - The wire format is identical to SoulMirror's: same relay, same envelope, same message
//     types — a light peer and a SoulMirror alter can message each other.
//   - No "alter semantics" are implemented: no auto-reply, no reading the protocol to make
//     decisions, no mission state machine. Whatever arrives is archived and reported to the
//     host through the OnEvent callback; what to answer is the host's call.
//
// cmd/soulnet layers a stdio JSON-RPC 2.0 server on top of it.
package peer

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// DefaultRelay is the public relay.
const DefaultRelay = "https://relay.startupworld.cn"

// Peer is one light-peer instance. The zero value is not usable; create it with Init.
type Peer struct {
	Home  string // data root (a2a/ lives underneath)
	Relay string // default relay (written into identity.json proxies when the identity is created; an existing identity uses its own proxies[0])

	Friends  *a2a.FriendStore
	Convs    *a2a.ConvStore
	Pendings *a2a.PendingStore
	Profiles *a2a.ProfileStore
	Groups   *a2a.GroupStore

	// OnEvent is called synchronously from the receive loop (one event at a time); the
	// host turns events into notifications. nil drops events. Keep the callback fast
	// (a slow callback stalls Poll→Ack).
	OnEvent func(Event)

	// Logf is the diagnostics sink (default log.Printf → stderr). stdout is reserved for
	// protocol frames.
	Logf func(format string, args ...any)

	// PresenceInterval > 0 makes Run poll every friend's presence at that interval and
	// emit presence.changed on changes. 0 (default) = no polling.
	PresenceInterval time.Duration

	mu sync.RWMutex
	id *a2a.Identity

	typMu   sync.Mutex
	typing  map[string]time.Time // peer → expiry of its "busy" (typing) mark
	presMu  sync.Mutex
	presCac map[string]presEntry

	// gvMu guards groupVoices: gid → member fp → that seat's announced agent names
	// (TypeGroupVoices metadata; in-memory — every host re-announces on start).
	gvMu        sync.Mutex
	groupVoices map[string]map[string][]string

	dlMu   sync.Mutex
	dlSeen map[string]struct{} // dead-letter ackIDs already logged

	rbMu   sync.Mutex
	rbSeen map[string]int // transient-retry budget (poison-mail guard, see retryBudget)

	// gkMu serializes every load→mutate→store of a group's keys.json: the receive loop
	// and API calls race on it, and a stale write must never resurrect an already-used
	// chain position (receivers reject the reuse as replay).
	gkMu sync.Mutex

	runMu   sync.Mutex
	running bool
}

type presEntry struct {
	online bool
	at     time.Time
}

// Init opens (or prepares) the peer data under home. It does not create an identity —
// call EnsureIdentity. An empty relay means DefaultRelay.
func Init(home, relay string) (*Peer, error) {
	if strings.TrimSpace(home) == "" {
		return nil, fmt.Errorf("home directory must not be empty")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	if relay == "" {
		relay = DefaultRelay
	}
	if err := os.MkdirAll(filepath.Join(abs, "a2a"), 0o755); err != nil {
		return nil, err
	}
	n := &Peer{
		Home:     abs,
		Relay:    strings.TrimRight(relay, "/"),
		Friends:  a2a.NewFriendStore(abs),
		Convs:    a2a.NewConvStore(abs),
		Pendings: a2a.NewPendingStore(abs),
		Profiles: a2a.NewProfileStore(abs),
		Groups:   a2a.NewGroupStore(abs),
		Logf:     log.Printf,
		typing:   map[string]time.Time{},
		presCac:  map[string]presEntry{},
		dlSeen:   map[string]struct{}{},
		rbSeen:   map[string]int{},
	}
	id, err := a2a.LoadIdentity(abs)
	if err != nil {
		return nil, err
	}
	n.id = id
	if id != nil {
		_ = a2a.EnsureProtocol(abs)
		_ = n.Friends.BackfillRead()
	}
	return n, nil
}

// Identity returns the local identity, or nil when none exists.
func (n *Peer) Identity() *a2a.Identity {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.id
}

// HasIdentity reports whether a local identity exists.
func (n *Peer) HasIdentity() bool { return n.Identity() != nil }

// EnsureIdentity creates an identity named name (proxies=[Relay]) when none exists;
// an existing one is returned untouched (no rename, no overwrite). name may be empty
// when an identity already exists.
func (n *Peer) EnsureIdentity(name string) (*a2a.Identity, error) {
	if id := n.Identity(); id != nil {
		return id, nil
	}
	return n.CreateIdentity(name)
}

// CreateIdentity generates a new identity and persists it (identity.json 0600 + default
// protocol.md). It fails with ErrIdentityExists when one already exists.
func (n *Peer) CreateIdentity(name string) (*a2a.Identity, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.id != nil {
		return nil, fmt.Errorf("%w (%s): refusing to overwrite", ErrIdentityExists, n.id.Fingerprint())
	}
	id, err := a2a.NewIdentity(n.Home, strings.TrimSpace(name), []string{n.Relay})
	if err != nil {
		return nil, err
	}
	if err := a2a.EnsureProtocol(n.Home); err != nil {
		return nil, err
	}
	n.id = id
	n.logf("identity created · name=%s fingerprint=%s relay=%s", id.Name, id.Fingerprint(), n.Relay)
	return id, nil
}

// SetIdentity installs an identity the host created, reloaded or renamed through its own
// code paths (a host that owns identity.json itself and only embeds the peer for group
// orchestration). The peer signs, encrypts and derives its fingerprint from it from now
// on. nil clears the identity.
func (n *Peer) SetIdentity(id *a2a.Identity) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.id = id
}

// Fingerprint returns the local fingerprint, or "" without an identity.
func (n *Peer) Fingerprint() string {
	if id := n.Identity(); id != nil {
		return id.Fingerprint()
	}
	return ""
}

// Card returns the local self-signed card.
func (n *Peer) Card() (*a2a.Card, error) {
	id := n.Identity()
	if id == nil {
		return nil, ErrNoIdentity
	}
	return id.Card()
}

// RelayBase returns the relay base URL actually in use: proxies[0] of the identity,
// otherwise Relay.
func (n *Peer) RelayBase() string {
	if id := n.Identity(); id != nil && len(id.Proxies) > 0 {
		return strings.TrimRight(id.Proxies[0], "/")
	}
	return n.Relay
}

// proxyClient returns the long-poll client for our own relay. Poll keeps the long-poll
// timeout; Ack (a short request) gets DeliverTimeout via a2a's split timeouts.
func (n *Peer) proxyClient() *a2a.ProxyClient {
	id := n.Identity()
	if id == nil {
		return nil
	}
	return a2a.NewProxyClient(n.RelayBase(), id).WithDeliverTimeout(DeliverTimeout)
}

func (n *Peer) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

func (n *Peer) emit(ev Event) {
	if n.OnEvent != nil {
		n.OnEvent(ev)
	}
}

// newMsgID delegates to the shared canonical generator (a2a.NewMessageID), so IDs are
// byte-for-byte the same shape as the SoulMirror daemon's.
func (n *Peer) newMsgID() string { return a2a.NewMessageID(n.Fingerprint()) }

// ——— errors ———

// Sentinel errors; the host maps them to JSON-RPC error codes (errors.Is).
var (
	ErrNoIdentity     = fmt.Errorf("no identity yet")
	ErrIdentityExists = fmt.Errorf("identity already exists")
	ErrNotFriend      = fmt.Errorf("not a friend")
	ErrSelf           = fmt.Errorf("cannot add yourself as a friend")
	ErrNoPending      = fmt.Errorf("no such pending friend request")
	ErrBadCard        = fmt.Errorf("invalid card")
	ErrBadProfile     = fmt.Errorf("invalid group profile")
	ErrNoProfile      = fmt.Errorf("no capability profile yet (profile.json)")
	ErrBadFile        = fmt.Errorf("attachment not usable")
	ErrArtifactSize   = fmt.Errorf("attachment exceeds the size limit")
	ErrNetwork        = fmt.Errorf("relay/directory request failed")
)

// ——— helpers ———

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
