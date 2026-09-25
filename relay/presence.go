// Device presence of the post office: when was each device of an identity last online?
//
// A frozen (non-active) device does not poll the mailbox, so "last long poll" cannot tell
// the owner whether the laptop in the drawer is still alive. Instead every owner-signed
// request (authBox: mail poll / ack, /box/*, /vault/*) that carries X-Soulnet-Device
// records seen[box][device] = now (and the X-Soulnet-Device-Name when present), and a
// frozen device keeps itself visible with a cheap heartbeat:
//
//	POST /box/seen {box, offline?} record presence only; ANY device of the box (never kicked);
//	                               offline=true is a clean-shutdown goodbye (sets Offline until the
//	                               device's next signed request)
//	GET  /box/devices?box=        {"devices":[{device, name, last_seen, active, offline}]} newest first
//
// POST /mail does not record: it is authenticated by the envelope signature, and a signed
// envelope can be re-posted by whoever holds a copy of it, so it proves nothing about the
// device that sends it.
//
// Presence lives in memory and is written to active/<box>.devices.json lazily -- at most
// once per presenceFlushEvery per mailbox (a timer flushes the tail) and on Flush (call it
// on shutdown). At most maxSeenDevices devices are kept per mailbox; the longest-unseen one
// is dropped first.
package relay

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

const (
	// presenceFlushEvery rate-limits presence writes per mailbox.
	presenceFlushEvery = time.Minute
	// maxSeenDevices bounds the devices remembered per mailbox.
	maxSeenDevices = 32
)

// boxPresence is the in-memory presence of one mailbox.
type boxPresence struct {
	devices   map[string]*a2a.DeviceSeen
	lastFlush time.Time
	dirty     bool
	scheduled bool
}

// presenceState is embedded in Server (see relay.go): psMu guards devSeen.
type presenceState struct {
	psMu       sync.Mutex
	devSeen    map[string]*boxPresence
	psFlushGap time.Duration // default presenceFlushEvery; 0 = write on every change (tests)
}

func (s *Server) presencePath(box string) string {
	return filepath.Join(s.activeDir(), box+".devices.json")
}

// presenceLocked returns box's presence, reading the file on first use. Caller holds psMu.
func (s *Server) presenceLocked(box string) *boxPresence {
	if bp := s.devSeen[box]; bp != nil {
		return bp
	}
	bp := &boxPresence{devices: map[string]*a2a.DeviceSeen{}}
	if raw, err := os.ReadFile(s.presencePath(box)); err == nil {
		var list []a2a.DeviceSeen
		if json.Unmarshal(raw, &list) == nil {
			for i := range list {
				d := list[i]
				d.Active = false // derived on read, never stored
				if ValidDeviceID(d.Device) {
					bp.devices[d.Device] = &d
				}
			}
		}
	}
	s.devSeen[box] = bp
	return bp
}

// noteDevice records that the device named in r's headers just talked to box (the caller
// has verified the owner signature). No-op without a (valid) device header.
func (s *Server) noteDevice(r *http.Request, box string) {
	device := strings.TrimSpace(r.Header.Get(a2a.HeaderDevice))
	if !ValidDeviceID(device) {
		return
	}
	name := trimDeviceName(r.Header.Get(a2a.HeaderDeviceName))
	now := time.Now().UTC()
	s.psMu.Lock()
	bp := s.presenceLocked(box)
	d := bp.devices[device]
	if d == nil {
		if len(bp.devices) >= maxSeenDevices { // make room: drop the longest-unseen device
			oldest := ""
			for id, x := range bp.devices {
				if oldest == "" || x.LastSeen.Before(bp.devices[oldest].LastSeen) {
					oldest = id
				}
			}
			delete(bp.devices, oldest)
		}
		d = &a2a.DeviceSeen{Device: device}
		bp.devices[device] = d
	}
	if name != "" {
		d.Name = name
	}
	d.LastSeen = now
	d.Offline = false // talking again: any goodbye it said earlier is stale
	bp.dirty = true
	wait := s.psFlushGap - time.Since(bp.lastFlush)
	if wait <= 0 {
		s.flushPresenceLocked(box, bp)
		s.psMu.Unlock()
		return
	}
	if !bp.scheduled {
		bp.scheduled = true
		time.AfterFunc(wait, func() {
			s.psMu.Lock()
			bp.scheduled = false
			s.flushPresenceLocked(box, bp)
			s.psMu.Unlock()
		})
	}
	s.psMu.Unlock()
}

// flushPresenceLocked writes box's presence if it changed. Caller holds psMu.
func (s *Server) flushPresenceLocked(box string, bp *boxPresence) {
	if !bp.dirty {
		return
	}
	list := make([]a2a.DeviceSeen, 0, len(bp.devices))
	for _, d := range bp.devices {
		list = append(list, *d)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Device < list[j].Device })
	raw, _ := json.Marshal(list)
	if err := writeFileAtomic(s.presencePath(box), raw); err != nil {
		log.Printf("[relay] presence box=%s flush failed: %v", a2a.ShortFp(box), err)
		return
	}
	bp.dirty, bp.lastFlush = false, time.Now()
}

// Flush persists state the core keeps in memory between lazy writes (device presence).
// Call it before the process exits; safe to call at any time.
func (s *Server) Flush() {
	s.psMu.Lock()
	defer s.psMu.Unlock()
	for box, bp := range s.devSeen {
		s.flushPresenceLocked(box, bp)
	}
}

// Devices returns the devices seen on box, most recently seen first, with Active set on
// the mailbox's active device (read access for extensions and tests).
func (s *Server) Devices(box string) []a2a.DeviceSeen {
	if !SafeBox(box) {
		return nil
	}
	s.psMu.Lock()
	bp := s.presenceLocked(box)
	out := make([]a2a.DeviceSeen, 0, len(bp.devices))
	for _, d := range bp.devices {
		out = append(out, *d)
	}
	s.psMu.Unlock()
	active := ""
	if ad := s.ActiveDevice(box); ad != nil {
		active = ad.Device
	}
	for i := range out {
		out[i].Active = out[i].Device == active
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Device < out[j].Device
	})
	return out
}

// mountPresence registers the presence routes; called from mountCore.
func (s *Server) mountPresence(must func(error)) {
	must(s.HandleFunc("POST /box/seen", s.boxSeen))
	must(s.HandleFunc("GET /box/devices", s.boxDevices))
}

// boxSeen: POST /box/seen {box} (owner-signed, X-Soulnet-Device required) records presence
// and nothing else -- deliberately NOT behind the active-device gate, so a frozen device
// can keep saying "I am still here".
func (s *Server) boxSeen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Box     string `json:"box"`
		Offline bool   `json:"offline"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		WriteError(w, 400, "request body must be {box, offline?}")
		return
	}
	if !SafeBox(body.Box) {
		WriteError(w, 400, "invalid box")
		return
	}
	if !ValidDeviceID(strings.TrimSpace(r.Header.Get(a2a.HeaderDevice))) {
		WriteError(w, 400, "X-Soulnet-Device header required")
		return
	}
	if err := s.authBox(r, "POST", "/box/seen", body.Box); err != nil { // records the presence
		WriteError(w, 401, err.Error())
		return
	}
	if body.Offline {
		s.markOffline(body.Box, strings.TrimSpace(r.Header.Get(a2a.HeaderDevice)))
	}
	WriteJSON(w, 200, map[string]any{"ok": true})
}

// markOffline records a clean-shutdown goodbye for device on box (after authBox has noted the
// request, so the entry exists). The flag is written through at once: a restart of the relay
// right after a goodbye must not bring the device back as "maybe online".
func (s *Server) markOffline(box, device string) {
	s.psMu.Lock()
	defer s.psMu.Unlock()
	bp := s.presenceLocked(box)
	d := bp.devices[device]
	if d == nil {
		return
	}
	d.Offline = true
	bp.dirty = true
	s.flushPresenceLocked(box, bp)
}

// boxDevices: GET /box/devices?box= (owner-signed) -> {"devices":[...]} most recent first.
func (s *Server) boxDevices(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	if err := s.authBox(r, "GET", "/box/devices", box); err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"devices": s.Devices(box)})
}
