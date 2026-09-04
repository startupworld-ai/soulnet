package peer

import (
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// Event kinds (one-to-one with the JSON-RPC notification method names of cmd/soulnet).
const (
	EventMessageReceived  = "message.received"  // text / app_share mail archived (attachment written to disk)
	EventFriendRequest    = "friend.request"    // friend request received and pended (pending/<id>.json)
	EventFriendAccepted   = "friend.accepted"   // the peer accepted my request; friend created
	EventTyping           = "typing"            // peer "busy" on/off (not archived)
	EventMissionUpdate    = "mission.update"    // task / mission_update / mission_bid mail archived
	EventArtifactReady    = "artifact.ready"    // chunked large file reassembled, verified and written
	EventPresenceChanged  = "presence.changed"  // friend presence changed (only when PresenceInterval>0)
	EventGroupMessage     = "group.message"     // group text archived (GID + sender in Peer)
	EventGroupTyping      = "group.typing"      // a member's seat is working in the group (Agent = which of their agents; not archived)
	EventGroupUpdated     = "group.updated"     // joined / roster changed / pins changed / left one group (GID)
	EventGroupApplication = "group.application" // a stranger applied to join a group I own (GID, Peer = applicant, Message = the group_join)
)

// Reasons carried by group.updated (Event.Reason). Hosts that only need "refetch the
// group" can ignore them; hosts that hang product behaviour on membership transitions
// (announce my seat agents on join, stop composing when removed, …) branch on them.
const (
	GroupReasonCreated  = "created"  // GroupCreate succeeded on this node
	GroupReasonJoined   = "joined"   // an invite landed and I joined a group I did not hold
	GroupReasonRejoined = "rejoined" // the owner re-admitted me after a removal (same conversation continues)
	GroupReasonRoster   = "roster"   // membership / governance profile changed (Added / Removed list the fingerprints)
	GroupReasonRemoved  = "removed"  // I was removed: the group stays on disk read-only (Peer.GroupLeft)
	GroupReasonLeft     = "left"     // I left the group, or deleted a removed group's local record
	GroupReasonPins     = "pins"     // the pinned announcements changed
	GroupReasonVoices   = "voices"   // a member announced its seat-agent names (Peer = who, Message = the announcement)
)

// Event is one notification produced by the receive loop. Fields are set per Kind.
type Event struct {
	Kind string    `json:"kind"`
	Peer string    `json:"peer"` // fingerprint of the other side
	TS   time.Time `json:"ts"`
	// GID names the group on group.message / group.updated.
	GID string `json:"gid,omitempty"`

	// Message: for message.received / mission.update the incoming mail (artifact base64
	// stripped; the file is at ArtifactPath); for friend.request the request message
	// (carries the peer's card).
	Message *a2a.Message `json:"message,omitempty"`
	// Seq is the 1-based line number of this message in conversations/<peer>/messages.jsonl
	// (set on archived kinds).
	Seq int `json:"seq,omitempty"`
	// ArtifactPath is the absolute path of the attachment on disk (message.received with an
	// attachment / artifact.ready).
	ArtifactPath string `json:"artifact_path,omitempty"`
	// ArtifactName / ArtifactID: set on artifact.ready.
	ArtifactName string `json:"artifact_name,omitempty"`
	ArtifactID   string `json:"artifact_id,omitempty"`

	// PendingID: on friend.request = request id (for friends.accept / reject).
	PendingID string `json:"pending_id,omitempty"`
	// Friend: on friend.accepted the newly created friend.
	Friend *a2a.Friend `json:"friend,omitempty"`

	// On: for typing / group.typing true=busy false=done; for presence.changed = online or not.
	On bool `json:"on"`
	// Agent: on group.typing the name of the sender's seat agent that is working ("" = their alter).
	Agent string `json:"agent,omitempty"`

	// Reason: on group.updated one of the GroupReason* constants (empty on events emitted
	// by older code paths; treat as "refetch").
	Reason string `json:"reason,omitempty"`
	// Added / Removed: on group.updated with Reason=roster the member fingerprints that
	// entered / left the roster in this update (sorted; either may be empty).
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}
