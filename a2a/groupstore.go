// GroupStore: local persistence of group membership and sender-key material.
//
// Layout (spec §12):
//
//	<baseDir>/a2a/groups/<gid>/group.json  — roster snapshot + join/read cursors (0644)
//	<baseDir>/a2a/groups/<gid>/keys.json   — MY sender chain + every sender's receive state (0600)
//
// The conversation archive of a group lives in the shared ConvStore under the key
// GroupConvKey(gid) — groups reuse the pairwise archive machinery unchanged.
package a2a

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GroupState is one group as this member sees it (group.json).
type GroupState struct {
	Roster     *GroupRoster `json:"roster"`
	JoinedAt   time.Time    `json:"joined_at"`
	LastReadAt time.Time    `json:"last_read_at,omitempty"`
}

// GroupKeys is the key material of one group (keys.json, 0600).
type GroupKeys struct {
	// Mine is my sending chain (nil until first initialized).
	Mine *GroupSenderKey `json:"mine,omitempty"`
	// Senders maps co-member fingerprint → my receive state for their chain.
	Senders map[string]*GroupRecvState `json:"senders,omitempty"`
	// DistributedTo maps co-member fingerprint → the epoch of MY chain they last received,
	// so sends know who still needs a `group_key` message.
	DistributedTo map[string]int `json:"distributed_to,omitempty"`
}

// GroupStore reads and writes the groups/ tree. One mutex serializes all writes (group
// traffic is low; correctness over throughput).
type GroupStore struct {
	base string
	mu   sync.Mutex
}

// NewGroupStore returns the store rooted at <baseDir>/a2a/groups.
func NewGroupStore(baseDir string) *GroupStore {
	return &GroupStore{base: filepath.Join(baseDir, "a2a", "groups")}
}

func (s *GroupStore) dir(gid string) string { return filepath.Join(s.base, SanitizeID(gid)) }

// Get returns the state of one group, or nil when this member does not have it.
func (s *GroupStore) Get(gid string) *GroupState {
	raw, err := os.ReadFile(filepath.Join(s.dir(gid), "group.json"))
	if err != nil {
		return nil
	}
	var st GroupState
	if json.Unmarshal(raw, &st) != nil || st.Roster == nil {
		return nil
	}
	return &st
}

// Put writes the state of one group.
func (s *GroupStore) Put(st *GroupState) error {
	if st == nil || st.Roster == nil || !ValidGroupID(st.Roster.GroupID) {
		return fmt.Errorf("group state needs a roster with a valid group id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.dir(st.Roster.GroupID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "group.json"), raw, 0o644)
}

// List returns every group on disk, sorted by group ID (deterministic).
func (s *GroupStore) List() []*GroupState {
	entries, err := os.ReadDir(s.base)
	if err != nil {
		return nil
	}
	out := make([]*GroupState, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st := s.Get(e.Name()); st != nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Roster.GroupID < out[j].Roster.GroupID })
	return out
}

// Keys returns the key material of one group (never nil; empty when absent).
func (s *GroupStore) Keys(gid string) *GroupKeys {
	raw, err := os.ReadFile(filepath.Join(s.dir(gid), "keys.json"))
	k := &GroupKeys{}
	if err == nil {
		_ = json.Unmarshal(raw, k)
	}
	if k.Senders == nil {
		k.Senders = map[string]*GroupRecvState{}
	}
	if k.DistributedTo == nil {
		k.DistributedTo = map[string]int{}
	}
	return k
}

// PutKeys writes the key material of one group (0600 — chain keys are secrets).
func (s *GroupStore) PutKeys(gid string, k *GroupKeys) error {
	if !ValidGroupID(gid) {
		return fmt.Errorf("invalid group id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.dir(gid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "keys.json"), raw, 0o600)
}

// MarkRead moves the read cursor of one group.
func (s *GroupStore) MarkRead(gid string, t time.Time) error {
	st := s.Get(gid)
	if st == nil {
		return fmt.Errorf("unknown group")
	}
	st.LastReadAt = t
	return s.Put(st)
}

// Remove deletes the group's state and key material (leaving / kicked). The conversation
// archive in ConvStore is intentionally kept, mirroring RemoveFriend.
func (s *GroupStore) Remove(gid string) error {
	if !ValidGroupID(gid) {
		return fmt.Errorf("invalid group id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.dir(gid))
}

// FindMemberCard scans every group for a member with fingerprint fp and returns its card
// (used to decrypt pairwise mail from co-members who are not friends).
func (s *GroupStore) FindMemberCard(fp string) *Card {
	for _, st := range s.List() {
		if c := st.Roster.Member(fp); c != nil {
			return c
		}
	}
	return nil
}

// ——— Pins (groups/<gid>/pins.json): the announcements on the group home ———

// GroupPin is one pinned announcement.
type GroupPin struct {
	ID   string    `json:"id"`   // the group_pin message id
	From string    `json:"from"` // who pinned it
	TS   time.Time `json:"ts"`
	Body string    `json:"body"`
}

func (s *GroupStore) pinsPath(gid string) string { return filepath.Join(s.dir(gid), "pins.json") }

// Pins returns the pinned announcements of one group, oldest first.
func (s *GroupStore) Pins(gid string) []GroupPin {
	raw, err := os.ReadFile(s.pinsPath(gid))
	if err != nil {
		return nil
	}
	var pins []GroupPin
	if json.Unmarshal(raw, &pins) != nil {
		return nil
	}
	return pins
}

func (s *GroupStore) writePins(gid string, pins []GroupPin) error {
	if err := os.MkdirAll(s.dir(gid), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.pinsPath(gid), raw, 0o644)
}

// AddPin appends one pin (idempotent on ID).
func (s *GroupStore) AddPin(gid string, pin GroupPin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pins := s.Pins(gid)
	for _, p := range pins {
		if p.ID == pin.ID {
			return nil
		}
	}
	return s.writePins(gid, append(pins, pin))
}

// RemovePin deletes the pin with the given id (idempotent).
func (s *GroupStore) RemovePin(gid, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pins := s.Pins(gid)
	kept := pins[:0]
	for _, p := range pins {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	return s.writePins(gid, kept)
}

// ——— Join applications (groups/<gid>/applications/<fp>.json, owner's node only) ———

// GroupApplication is one stranger's pending request to join (group_join).
type GroupApplication struct {
	Card *Card     `json:"card"`
	Note string    `json:"note,omitempty"`
	TS   time.Time `json:"ts"`
	// Payment is the paid-join proof (join policy "paid"), passed through from
	// the group_join message; nil for invite/apply joins.
	Payment *JoinPayment `json:"payment,omitempty"`
}

func (s *GroupStore) appsDir(gid string) string { return filepath.Join(s.dir(gid), "applications") }

// PutApplication stores/overwrites one application, keyed by the applicant fingerprint.
func (s *GroupStore) PutApplication(gid string, app *GroupApplication) error {
	if app == nil || app.Card == nil {
		return fmt.Errorf("application needs a card")
	}
	fp, err := app.Card.Fingerprint()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.appsDir(gid), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.appsDir(gid), SanitizeID(fp)+".json"), raw, 0o644)
}

// Applications lists the pending join applications of one group (by file order).
func (s *GroupStore) Applications(gid string) []*GroupApplication {
	entries, err := os.ReadDir(s.appsDir(gid))
	if err != nil {
		return nil
	}
	out := make([]*GroupApplication, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.appsDir(gid), e.Name()))
		if err != nil {
			continue
		}
		var app GroupApplication
		if json.Unmarshal(raw, &app) == nil && app.Card != nil {
			out = append(out, &app)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// RemoveApplication deletes one application (approved or rejected).
func (s *GroupStore) RemoveApplication(gid, fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(filepath.Join(s.appsDir(gid), SanitizeID(fp)+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ——— Paid-join consumed-tx ledger (groups/<gid>/consumed-tx.json, owner's node) ———

// ConsumePaymentTx atomically claims one on-chain payment for one group: the
// first caller gets true, any later caller for the same tx_hash gets false.
// This is the replay guard — a single USDC transfer admits exactly one member,
// so an applicant cannot reuse their own (or anyone else's) tx_hash for a
// second admission. Claims are made at approve time, only when a member is
// actually admitted.
func (s *GroupStore) ConsumePaymentTx(gid, txHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	consumed := s.consumedTx(gid)
	if consumed[txHash] {
		return false, nil
	}
	consumed[txHash] = true
	return true, s.writeConsumedTx(gid, consumed)
}

// consumedTx loads the group's consumed-tx set (caller holds s.mu).
func (s *GroupStore) consumedTx(gid string) map[string]bool {
	raw, err := os.ReadFile(filepath.Join(s.dir(gid), "consumed-tx.json"))
	if err != nil {
		return map[string]bool{}
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(list))
	for _, h := range list {
		out[h] = true
	}
	return out
}

// writeConsumedTx persists the consumed-tx set as a sorted list (caller holds s.mu).
func (s *GroupStore) writeConsumedTx(gid string, consumed map[string]bool) error {
	list := make([]string, 0, len(consumed))
	for h := range consumed {
		list = append(list, h)
	}
	sort.Strings(list)
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(gid), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir(gid), "consumed-tx.json"), raw, 0o644)
}
