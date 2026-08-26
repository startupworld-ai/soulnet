// Group endpoints (A2A wire spec §14.4): the relay stores the owner-signed roster (the
// member list IS shared information, so it lives here as the single source of truth) and
// fans group envelopes out into the members' existing mailboxes. Content stays ciphertext
// end to end — the relay learns membership, never messages.
package relay

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/startupworld-ai/soulnet/a2a"
)

// Group event kinds (on top of the core kinds in ext.go).
const (
	EventGroupPublished = "group.published" // FP = owner fingerprint; Data["gid"], Data["version"]
	EventGroupMail      = "group.mail"      // FP = sender fingerprint; Data["gid"], Data["delivered"]
)

const maxRosterBytes = 256 << 10 // 256KB: 128 members x ~1KB card, with headroom

func (s *Server) groupPath(gid string) string {
	return filepath.Join(s.dataDir, "groups", gid+".json")
}

// loadRoster reads a stored roster; nil when absent or unreadable.
func (s *Server) loadRoster(gid string) *a2a.GroupRoster {
	if !a2a.ValidGroupID(gid) {
		return nil
	}
	raw, err := os.ReadFile(s.groupPath(gid))
	if err != nil {
		return nil
	}
	var g a2a.GroupRoster
	if json.Unmarshal(raw, &g) != nil {
		return nil
	}
	return &g
}

// mountGroups registers the group routes; called from mountCore.
func (s *Server) mountGroups(must func(error)) {
	must(s.HandleFunc("POST /group/publish", s.groupPublish))
	must(s.HandleFunc("GET /group/fetch", s.groupFetch))
	must(s.HandleFunc("POST /group/mail", s.groupMail))
	must(s.HandleFunc("GET /group/card", s.groupCard))
	must(s.HandleFunc("GET /group/search", s.groupSearch))
}

// groupCard returns the PUBLIC card of a group (no auth): only what the owner opted
// into by setting profile.public. Non-public and unknown groups both answer 404.
func (s *Server) groupCard(w http.ResponseWriter, r *http.Request) {
	gid := r.URL.Query().Get("gid")
	if !a2a.ValidGroupID(gid) {
		WriteError(w, 400, "invalid gid")
		return
	}
	g := s.loadRoster(gid)
	if g == nil {
		WriteError(w, 404, "unknown group")
		return
	}
	card := g.PublicCard()
	if card == nil {
		WriteError(w, 404, "group is not public")
		return
	}
	WriteJSON(w, 200, map[string]any{"card": card})
}

// groupSearch lists public groups matching a keyword/tag (no auth; linear scan — fine
// at self-hosted relay scale).
func (s *Server) groupSearch(w http.ResponseWriter, r *http.Request) {
	kw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kw")))
	tag := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tag")))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "groups"))
	if err != nil && !os.IsNotExist(err) {
		WriteError(w, 500, err.Error())
		return
	}
	out := make([]*a2a.GroupCard, 0, limit)
	for _, e := range entries {
		if len(out) >= limit {
			break
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || name == e.Name() {
			continue
		}
		g := s.loadRoster(name)
		if g == nil {
			continue
		}
		card := g.PublicCard()
		if card == nil {
			continue
		}
		if tag != "" {
			hit := false
			for _, t := range card.Tags {
				if strings.ToLower(t) == tag {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		if kw != "" {
			hay := strings.ToLower(card.Name + "\n" + card.RulesHead + "\n" + strings.Join(card.Tags, "\n"))
			if !strings.Contains(hay, kw) {
				continue
			}
		}
		out = append(out, card)
	}
	WriteJSON(w, 200, map[string]any{"groups": out})
}

// groupPublish stores a new roster version. Authorization is the roster itself: it must
// verify against the owner key, and a republish must keep the owner and increase the
// version (so neither a member nor a bystander can overwrite a group).
func (s *Server) groupPublish(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRosterBytes))
	if err != nil {
		WriteError(w, 400, "failed to read request body")
		return
	}
	var g a2a.GroupRoster
	if err := json.Unmarshal(raw, &g); err != nil {
		WriteError(w, 400, "invalid roster JSON")
		return
	}
	if err := g.Verify(); err != nil {
		WriteError(w, 401, "roster verification failed: "+err.Error())
		return
	}
	if !s.rateLimit(g.OwnerPub) {
		WriteError(w, 429, "publishing too frequently")
		return
	}
	s.grMu.Lock()
	defer s.grMu.Unlock()
	if old := s.loadRoster(g.GroupID); old != nil {
		if old.OwnerPub != g.OwnerPub {
			WriteError(w, 403, "group is owned by a different key")
			return
		}
		if g.Version <= old.Version {
			WriteError(w, 409, "roster version must increase")
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.groupPath(g.GroupID)), 0o755); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	stored, _ := json.Marshal(&g)
	if err := os.WriteFile(s.groupPath(g.GroupID), stored, 0o644); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	s.emit(Event{Kind: EventGroupPublished, FP: g.OwnerFp(), Data: map[string]any{"gid": g.GroupID, "version": g.Version}})
	log.Printf("[relay-debug] groupPublish gid=%s owner=%s version=%d members=%d", a2a.ShortFp(g.GroupID), a2a.ShortFp(g.OwnerFp()), g.Version, len(g.Members))
	WriteJSON(w, 200, map[string]any{"ok": true, "version": g.Version})
}

// groupFetch returns the current roster to MEMBERS (signed request; the member list is
// shared within the group, not with the world).
func (s *Server) groupFetch(w http.ResponseWriter, r *http.Request) {
	gid := r.URL.Query().Get("gid")
	if !a2a.ValidGroupID(gid) {
		WriteError(w, 400, "invalid gid")
		return
	}
	fp, err := VerifyRequest(r, "GET", "/group/fetch")
	if err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	g := s.loadRoster(gid)
	if g == nil {
		WriteError(w, 404, "unknown group")
		return
	}
	if g.Member(fp) == nil {
		log.Printf("[relay-debug] groupFetch gid=%s by=%s DENIED (not a member)", a2a.ShortFp(gid), a2a.ShortFp(fp))
		WriteError(w, 403, "not a member of this group")
		return
	}
	log.Printf("[relay-debug] groupFetch gid=%s by=%s OK version=%d", a2a.ShortFp(gid), a2a.ShortFp(fp), g.Version)
	WriteJSON(w, 200, map[string]any{"roster": g})
}

// groupMail fans one group envelope out: verify the sender signature, check membership
// against the stored roster, then copy the ciphertext into every OTHER member's mailbox
// (To stamped per copy; the signature covers gid+ts+cipher, so stamping does not touch
// signed bytes). Receivers re-verify both signature and membership — the relay is a
// convenience, not a trust anchor.
func (s *Server) groupMail(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteError(w, 400, "failed to read request body")
		return
	}
	var env a2a.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		WriteError(w, 400, "invalid envelope JSON")
		return
	}
	senderFp, err := env.VerifyGroupEnvelope()
	if err != nil {
		WriteError(w, 401, "group envelope check failed: "+err.Error())
		return
	}
	if !s.rateLimit(env.From) {
		WriteError(w, 429, "sending too frequently")
		return
	}
	g := s.loadRoster(env.GID)
	if g == nil {
		WriteError(w, 404, "unknown group")
		return
	}
	if g.Member(senderFp) == nil {
		WriteError(w, 403, "sender is not a member of this group")
		return
	}
	delivered := 0
	var firstErr error
	for _, fp := range g.MemberFps() {
		if fp == senderFp || !SafeBox(fp) {
			continue
		}
		copyEnv := env
		copyEnv.To = fp
		if err := s.deliver(&copyEnv); err != nil {
			log.Printf("[relay-debug] groupMail gid=%s sender=%s member=%s FAIL err=%v", a2a.ShortFp(env.GID), a2a.ShortFp(senderFp), a2a.ShortFp(fp), err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		log.Printf("[relay-debug] groupMail gid=%s sender=%s member=%s OK", a2a.ShortFp(env.GID), a2a.ShortFp(senderFp), a2a.ShortFp(fp))
		delivered++
	}
	log.Printf("[relay-debug] groupMail gid=%s sender=%s members=%d delivered=%d", a2a.ShortFp(env.GID), a2a.ShortFp(senderFp), len(g.MemberFps()), delivered)
	if delivered == 0 && firstErr != nil {
		WriteError(w, 500, firstErr.Error())
		return
	}
	s.emit(Event{Kind: EventGroupMail, FP: senderFp, Data: map[string]any{"gid": env.GID, "delivered": delivered}})
	WriteJSON(w, 200, map[string]any{"ok": true, "delivered": delivered})
}
