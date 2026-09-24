// Device sessions of the post office: one identity, many devices, one ACTIVE device per
// mailbox at a time.
//
// The relay keeps one active/<fingerprint>.json per mailbox ({device, name, since}). Only
// the active device may poll, acknowledge or send AS this identity; every other device of
// the same key gets 409 {"error":"kicked"} and is expected to freeze. Taking over is one
// explicit call (POST /box/active) and wakes the mailbox's long-poller so the previous
// device learns about it immediately.
//
// The device id is not a credential -- requests are still signed with the identity key --
// so anyone holding the private key can pose as any device. That is the intended threat
// model: the private key IS the identity; device sessions only stop two honest copies of
// it from fighting over the same mailbox.
//
// Legacy clients (no X-Soulnet-Device header) keep working exactly as before as long as
// no device has ever claimed the mailbox; once one has, they are kicked like any other
// non-active device (an upgraded device is active, the old copy must not steal mail).
package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// EventBoxActiveChanged is emitted when a mailbox's active device changes (first claim or
// takeover). FP = mailbox; Data["device"], Data["name"], Data["previous"] (previous device
// id, "" on first claim).
const EventBoxActiveChanged = "box.active_changed"

// deviceIDRe bounds a device id: opaque token, URL/header safe, 1..128 characters.
var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9._~=-]{1,128}$`)

// maxDeviceName caps the human-readable device name.
const maxDeviceName = 64

// ValidDeviceID reports whether s is acceptable as a device id.
func ValidDeviceID(s string) bool { return deviceIDRe.MatchString(s) }

func (s *Server) activeDir() string { return filepath.Join(s.dataDir, "active") }

func (s *Server) activePath(box string) string { return filepath.Join(s.activeDir(), box+".json") }

// loadActiveLocked returns the mailbox's active device (nil = none), reading the file on a
// cache miss. Caller holds acMu.
func (s *Server) loadActiveLocked(box string) *a2a.ActiveDevice {
	if ad, ok := s.active[box]; ok {
		return ad
	}
	var ad *a2a.ActiveDevice
	if raw, err := os.ReadFile(s.activePath(box)); err == nil {
		var v a2a.ActiveDevice
		if json.Unmarshal(raw, &v) == nil && v.Device != "" {
			ad = &v
		}
	}
	s.active[box] = ad
	return ad
}

// ActiveDevice returns the device currently holding the mailbox, or nil when none has
// ever claimed it (read access for extensions and tests).
func (s *Server) ActiveDevice(box string) *a2a.ActiveDevice {
	if !SafeBox(box) {
		return nil
	}
	s.acMu.Lock()
	defer s.acMu.Unlock()
	if ad := s.loadActiveLocked(box); ad != nil {
		cp := *ad
		return &cp
	}
	return nil
}

// setActiveLocked persists a new active device for box and returns the previous one.
// Caller holds acMu.
func (s *Server) setActiveLocked(box string, ad a2a.ActiveDevice) (prev *a2a.ActiveDevice, err error) {
	prev = s.loadActiveLocked(box)
	if err := os.MkdirAll(s.activeDir(), 0o755); err != nil {
		return prev, err
	}
	raw, _ := json.Marshal(&ad)
	if err := os.WriteFile(s.activePath(box), raw, 0o644); err != nil {
		return prev, err
	}
	s.active[box] = &ad
	return prev, nil
}

// claimActive makes device the active device of box (explicit takeover): persists it,
// wakes the mailbox's long-poller (so the previous device gets its 409 now, not after its
// current poll times out) and emits EventBoxActiveChanged. Claiming with the device that
// is already active only refreshes the name and is not an event.
func (s *Server) claimActive(box, device, name, handoff string) (*a2a.ActiveDevice, error) {
	s.acMu.Lock()
	cur := s.loadActiveLocked(box)
	if cur != nil && cur.Device == device {
		// Re-claim by the holder: refresh the name and the handoff note (a new takeover
		// attempt from the same device may carry a fresh rendezvous).
		if (cur.Name != name && name != "") || cur.Handoff != handoff {
			upd := *cur
			if name != "" {
				upd.Name = name
			}
			upd.Handoff = handoff
			if _, err := s.setActiveLocked(box, upd); err != nil {
				s.acMu.Unlock()
				return nil, err
			}
			cur = &upd
		}
		cp := *cur
		s.acMu.Unlock()
		return &cp, nil
	}
	ad := a2a.ActiveDevice{Device: device, Name: name, Since: time.Now().UTC(), Handoff: handoff}
	prev, err := s.setActiveLocked(box, ad)
	s.acMu.Unlock()
	if err != nil {
		return nil, err
	}
	s.wake(box)
	prevID := ""
	if prev != nil {
		prevID = prev.Device
	}
	s.emit(Event{Kind: EventBoxActiveChanged, FP: box, Data: map[string]any{"device": device, "name": name, "previous": prevID}})
	return &ad, nil
}

// deviceGate applies the active-device rule to one mailbox-owner request on box and
// returns the device that owns the mailbox when the caller must be refused (nil = pass).
//
//	header present, no active device  -> the caller becomes active (implicit first claim)
//	header present, equals active     -> pass
//	header present, differs           -> kicked
//	no header (legacy), no active     -> pass (unchanged legacy behaviour)
//	no header (legacy), active set    -> kicked
func (s *Server) deviceGate(r *http.Request, box string) *a2a.ActiveDevice {
	device := strings.TrimSpace(r.Header.Get(a2a.HeaderDevice))
	s.acMu.Lock()
	cur := s.loadActiveLocked(box)
	if cur == nil {
		if device == "" || !ValidDeviceID(device) {
			s.acMu.Unlock()
			return nil // legacy caller on a never-claimed mailbox (a malformed id counts as none)
		}
		name := trimDeviceName(r.Header.Get(a2a.HeaderDeviceName))
		ad := a2a.ActiveDevice{Device: device, Name: name, Since: time.Now().UTC()}
		_, err := s.setActiveLocked(box, ad)
		s.acMu.Unlock()
		if err == nil {
			s.emit(Event{Kind: EventBoxActiveChanged, FP: box, Data: map[string]any{"device": device, "name": name, "previous": ""}})
		}
		return nil
	}
	cp := *cur
	s.acMu.Unlock()
	if device == cp.Device {
		return nil
	}
	return &cp
}

// writeKicked answers the 409 kicked verdict for the given active device.
func writeKicked(w http.ResponseWriter, ad *a2a.ActiveDevice) {
	WriteJSON(w, http.StatusConflict, map[string]any{
		"error":         a2a.KickedErrorCode,
		"active_device": ad.Device,
		"active_name":   ad.Name,
		"since":         ad.Since.UTC().Format(time.RFC3339),
		"handoff":       ad.Handoff, // opaque note from the claimer; empty when none
	})
}

func trimDeviceName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > maxDeviceName {
		name = name[:maxDeviceName]
	}
	return name
}

// mountDevice registers the device-session routes; called from mountCore.
func (s *Server) mountDevice(must func(error)) {
	must(s.HandleFunc("POST /box/active", s.boxActiveClaim))
	must(s.HandleFunc("GET /box/active", s.boxActiveGet))
}

// boxActiveClaim: POST /box/active {box, device, name} (signed by the mailbox owner) makes
// device the active device of box and kicks whatever device held it before.
func (s *Server) boxActiveClaim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Box     string `json:"box"`
		Device  string `json:"device"`
		Name    string `json:"name"`
		Handoff string `json:"handoff"` // opaque note for the kicked device, <= a2a.MaxHandoffBytes
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		WriteError(w, 400, "request body must be {box, device, name, handoff?}")
		return
	}
	if len(body.Handoff) > a2a.MaxHandoffBytes {
		WriteError(w, 400, "handoff too large")
		return
	}
	if !SafeBox(body.Box) {
		WriteError(w, 400, "invalid box")
		return
	}
	body.Device = strings.TrimSpace(body.Device)
	if !ValidDeviceID(body.Device) {
		WriteError(w, 400, "invalid device id")
		return
	}
	if err := s.authBox(r, "POST", "/box/active", body.Box); err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	ad, err := s.claimActive(body.Box, body.Device, trimDeviceName(body.Name), body.Handoff)
	if err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "active": ad})
}

// boxActiveGet: GET /box/active?box= (signed by the mailbox owner) -> {"active": {...} | null}.
func (s *Server) boxActiveGet(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	if err := s.authBox(r, "GET", "/box/active", box); err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"active": s.ActiveDevice(box)})
}

// senderBox returns the mailbox (fingerprint) of an envelope's signer, "" when the From
// key is malformed (VerifyEnvelope has already rejected that case).
func senderBox(env *a2a.Envelope) string {
	pub, err := a2a.DecodeKey(env.From)
	if err != nil || len(pub) != 32 {
		return ""
	}
	return a2a.Fingerprint(pub)
}
