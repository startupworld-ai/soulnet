package peer

import (
	"context"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// presenceTimeout bounds each GET /presence (a short request; see a2a.ProxyClient.Presence).
const presenceTimeout = 5 * time.Second

// presenceTTL is how long a presence answer is cached.
const presenceTTL = 10 * time.Second

// Presence reports whether each of the given friends is online (long-polled its relay in
// the last 75s). Non-friends / no card → false. Results are cached for 10s. Empty fps =
// all friends.
func (n *Peer) Presence(fps []string) map[string]bool {
	if len(fps) == 0 {
		for _, fr := range n.Friends.Friends() {
			fps = append(fps, fr.Fingerprint)
		}
	}
	out := make(map[string]bool, len(fps))
	for _, fp := range fps {
		out[fp] = n.peerOnline(n.Friends.Get(fp))
	}
	return out
}

// PeerOnline reports whether one friend is online (long-polled its relay in the last
// 75s); the answer is cached for 10s. Non-friend / no card → false.
func (n *Peer) PeerOnline(fp string) bool { return n.peerOnline(n.Friends.Get(fp)) }

func (n *Peer) peerOnline(fr *a2a.Friend) bool {
	if fr == nil || fr.Card == nil || len(fr.Card.Proxies) == 0 {
		return false
	}
	n.presMu.Lock()
	if e, ok := n.presCac[fr.Fingerprint]; ok && time.Since(e.at) < presenceTTL {
		n.presMu.Unlock()
		return e.online
	}
	n.presMu.Unlock()
	online := false
	for _, base := range fr.Card.Proxies {
		pc := a2a.NewProxyClient(base, n.Identity()).WithDeliverTimeout(presenceTimeout)
		got, _ := pc.Presence(context.Background(), []string{fr.Fingerprint}) // error == offline on that relay
		if got[fr.Fingerprint] {
			online = true
			break
		}
	}
	n.presMu.Lock()
	n.presCac[fr.Fingerprint] = presEntry{online: online, at: time.Now()}
	n.presMu.Unlock()
	return online
}

// presenceWatch polls every friend's presence at PresenceInterval and emits
// presence.changed on changes.
func (n *Peer) presenceWatch(ctx context.Context) {
	last := map[string]bool{}
	t := time.NewTicker(n.PresenceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, fr := range n.Friends.Friends() {
			on := n.peerOnline(fr)
			prev, seen := last[fr.Fingerprint]
			last[fr.Fingerprint] = on
			if seen && prev == on {
				continue
			}
			if !seen && !on {
				continue // stay quiet about offline friends on the first round
			}
			n.emit(Event{Kind: EventPresenceChanged, Peer: fr.Fingerprint, TS: time.Now(), On: on})
		}
	}
}
