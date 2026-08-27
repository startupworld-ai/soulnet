// JSON-RPC methods for groups (A2A wire spec §14). Registered in methods() in rpc.go.
package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/peer"
)

func (s *Server) groupCreate(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Name    string            `json:"name"`
		Members []string          `json:"members"`
		Profile *a2a.GroupProfile `json:"profile"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, invalid("name must not be empty")
	}
	if len(p.Members) == 0 {
		return nil, invalid("members must name at least one friend fingerprint")
	}
	v, err := s.n.GroupCreate(ctx, p.Name, p.Members, p.Profile)
	if err != nil {
		return nil, err
	}
	return map[string]any{"group": v}, nil
}

func (s *Server) groupList(context.Context, json.RawMessage) (any, error) {
	return map[string]any{"groups": s.n.GroupList()}, nil
}

func (s *Server) groupGet(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	v, err := s.n.GroupInfo(p.GID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"group": v}, nil
}

func (s *Server) groupSend(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID  string `json:"gid"`
		Body string `json:"body"`
		// By is the provenance of the post: "owner" (default) or "alter"; enforced
		// against the group profile. Auto marks the alter's automatic posts. Agent
		// names which seat agent composed a by=alter post (display provenance).
		By    string `json:"by"`
		Auto  bool   `json:"auto"`
		Agent string `json:"agent"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	res, err := s.n.GroupSend(ctx, p.GID, p.Body, peer.GroupSendOptions{By: p.By, Auto: p.Auto, Agent: p.Agent})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// groupVoicesAnnounce fans out this seat's enabled agent names in one group
// (metadata for the other members' @-autocomplete; empty voices clears it).
func (s *Server) groupVoicesAnnounce(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID    string   `json:"gid"`
		Voices []string `json:"voices"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupAnnounceVoices(ctx, p.GID, p.Voices)
}

// groupTyping fans out a "this seat is working here" signal (presence metadata,
// not archived; agent = which of my seat agents, "" = the alter).
func (s *Server) groupTyping(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID   string `json:"gid"`
		On    *bool  `json:"on"`
		Agent string `json:"agent"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	on := p.On == nil || *p.On
	return map[string]any{"ok": true}, s.n.GroupTyping(ctx, p.GID, on, p.Agent)
}

func (s *Server) groupConversation(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID   string `json:"gid"`
		Since int    `json:"since"`
		Limit int    `json:"limit"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	return map[string]any{"entries": s.n.GroupConversation(p.GID, p.Since, p.Limit)}, nil
}

func (s *Server) groupMarkRead(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
		Seq int    `json:"seq"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupMarkRead(p.GID, p.Seq)
}

func (s *Server) groupLeave(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupLeave(ctx, p.GID)
}

func (s *Server) groupKick(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
		FP  string `json:"fp"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" || p.FP == "" {
		return nil, invalid("gid and fp must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupKick(ctx, p.GID, p.FP)
}

func (s *Server) groupSetProfile(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID     string            `json:"gid"`
		Profile *a2a.GroupProfile `json:"profile"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	if p.Profile == nil {
		return nil, invalid("profile must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupSetProfile(ctx, p.GID, p.Profile)
}

func (s *Server) groupPin(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID  string `json:"gid"`
		Body string `json:"body"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	pin, err := s.n.GroupPin(ctx, p.GID, p.Body)
	if err != nil {
		return nil, err
	}
	return map[string]any{"pin": pin}, nil
}

func (s *Server) groupUnpin(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
		ID  string `json:"id"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" || p.ID == "" {
		return nil, invalid("gid and id must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupUnpin(ctx, p.GID, p.ID)
}

func (s *Server) groupLookup(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.URI) == "" {
		return nil, invalid("uri must not be empty")
	}
	card, err := s.n.GroupLookup(ctx, p.URI)
	if err != nil {
		return nil, err
	}
	return map[string]any{"card": card}, nil
}

func (s *Server) groupApply(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		URI     string           `json:"uri"`
		Note    string           `json:"note"`
		Payment *a2a.JoinPayment `json:"payment,omitempty"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.URI) == "" {
		return nil, invalid("uri must not be empty")
	}
	gid, err := s.n.GroupApply(ctx, p.URI, p.Note, p.Payment)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "gid": gid}, nil
}

func (s *Server) groupApplications(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" {
		return nil, invalid("gid must not be empty")
	}
	apps, err := s.n.GroupApplications(p.GID)
	if err != nil {
		return nil, err
	}
	if apps == nil {
		apps = []peer.GroupApplicationView{}
	}
	return map[string]any{"applications": apps}, nil
}

func (s *Server) groupApprove(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
		FP  string `json:"fp"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" || p.FP == "" {
		return nil, invalid("gid and fp must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupApprove(ctx, p.GID, p.FP)
}

func (s *Server) groupApplicationReject(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
		FP  string `json:"fp"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" || p.FP == "" {
		return nil, invalid("gid and fp must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupRejectApplication(p.GID, p.FP)
}

func (s *Server) groupInvite(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		GID string `json:"gid"`
		FP  string `json:"fp"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.GID == "" || p.FP == "" {
		return nil, invalid("gid and fp must not be empty")
	}
	return map[string]any{"ok": true}, s.n.GroupInvite(ctx, p.GID, p.FP)
}
