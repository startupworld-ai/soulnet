// Presence over a long-lived connection: a device that holds GET /box/presence open is online;
// the moment that connection ends the device is Offline.
//
// Why not just the heartbeat: POST /box/seen only says "I was here at T". A peer deciding
// whether to wait for a device (e.g. a takeover waiting for the old center to hand its data
// over) has to guess from how old T is, which means waiting a minute. With a held connection
// the relay knows at once when the process exits, crashes or its network goes away (the TCP
// connection closes), and within about two ping intervals when the machine just stops
// answering (lid closed, power cut) -- the relay pings, the client's WebSocket layer answers
// automatically, and a read deadline of 2 x ping + 2s catches the silence.
//
//	GET /box/presence?box=&ping=15s   WebSocket; owner-signed like /box/seen, X-Soulnet-Device
//	                                  required; ping is the server ping interval the client
//	                                  asks for (bounded [minPresencePing, maxPresencePing],
//	                                  default defaultPresencePing) -- a phone asks for a
//	                                  longer one to save battery.
//
// While at least one presence connection of a device is open, GET /box/devices reports it
// Connected with LastSeen = now. When its last connection ends it is marked Offline (written
// through, like a goodbye). Any later signed request clears Offline as usual.
package relay

import (
	"net/http"
	"strings"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/ws"
)

var (
	// minPresencePing / maxPresencePing bound the ping interval a client may ask for.
	// Tests lower minPresencePing to exercise the timeout quickly.
	minPresencePing     = 5 * time.Second
	maxPresencePing     = 60 * time.Second
	defaultPresencePing = 15 * time.Second
	// presenceSlack is added to 2 x ping for the read deadline (network jitter).
	presenceSlack = 2 * time.Second
)

// presenceConnsLocked returns the open presence connection counts of box. Caller holds psMu.
func (s *Server) presenceConnsLocked(box string) map[string]int {
	if s.psConns == nil {
		s.psConns = map[string]map[string]int{}
	}
	m := s.psConns[box]
	if m == nil {
		m = map[string]int{}
		s.psConns[box] = m
	}
	return m
}

// connectedLocked reports whether device holds an open presence connection on box. Caller holds psMu.
func (s *Server) connectedLocked(box, device string) bool {
	return s.psConns != nil && s.psConns[box][device] > 0
}

func parsePresencePing(v string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return defaultPresencePing
	}
	if d < minPresencePing {
		return minPresencePing
	}
	if d > maxPresencePing {
		return maxPresencePing
	}
	return d
}

// boxPresenceConn: GET /box/presence?box=&ping= upgrades to a WebSocket held open while the device is up.
func (s *Server) boxPresenceConn(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	device := strings.TrimSpace(r.Header.Get(a2a.HeaderDevice))
	if !ValidDeviceID(device) {
		WriteError(w, 400, "X-Soulnet-Device header required")
		return
	}
	if !ws.IsWebSocket(r) {
		WriteError(w, 400, "websocket upgrade required")
		return
	}
	if err := s.authBox(r, "GET", "/box/presence", box); err != nil { // records presence, clears Offline
		WriteError(w, 401, err.Error())
		return
	}
	ping := parsePresencePing(r.URL.Query().Get("ping"))
	conn, err := ws.Accept(w, r)
	if err != nil {
		return
	}
	s.psMu.Lock()
	s.presenceConnsLocked(box)[device]++
	s.psMu.Unlock()

	conn.SetReadTimeout(2*ping + presenceSlack)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(ping)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if conn.Ping() != nil {
					return
				}
			}
		}
	}()
	for {
		// ReadMessage answers pings and swallows pongs; every frame renews the read deadline.
		// It only returns on a data frame (ignored), a close frame, or a read error / timeout.
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	close(done)
	_ = conn.Close()

	s.psMu.Lock()
	m := s.presenceConnsLocked(box)
	m[device]--
	last := m[device] <= 0
	if last {
		delete(m, device)
		bp := s.presenceLocked(box)
		if d := bp.devices[device]; d != nil {
			d.LastSeen = time.Now().UTC()
			d.Offline = true
			bp.dirty = true
			s.flushPresenceLocked(box, bp)
		}
	}
	s.psMu.Unlock()
}
