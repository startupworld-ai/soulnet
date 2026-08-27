/**
 * NetworkClient — the ONE interface every SoulMirror network backend
 * implements (architecture spec §2). Host plugins only ever see this
 * interface. Two implementations ship:
 *
 *  - `soulnet` (default): spawns the `soulnet` light peer (Go, ../cmd/soulnet)
 *    and talks line-delimited JSON-RPC 2.0 over its stdio (./soulnet.ts);
 *  - `fake`: an in-memory backend for tests and UI work (./fake.ts).
 */
import type { A2AMessageId, Fingerprint } from '../events.ts'

export type BackendKind = 'fake' | 'soulnet'

export interface Identity {
  readonly fp: Fingerprint
  readonly name: string
  /** Card URI (`soulmirror://card?...`) others paste to add us. */
  readonly cardUri: string
  /** ISO timestamp of identity creation when known. */
  readonly createdAt?: string
}

export interface Friend {
  readonly fp: Fingerprint
  /** Display name: the local note when set, else the name on the peer's card. */
  readonly name: string
  /** Local note (SoulMirror "note"); undefined when none was set. */
  readonly remark?: string
  /** Name the peer declared on its card. */
  readonly cardName?: string
  /** Per-friend diplomacy protocol override (friends.yaml `protocol`); undefined/empty = the global protocol applies. */
  readonly protocol?: string
  readonly online?: boolean
  /** Unread inbound messages (sent by the peer after our read cursor). */
  readonly unread: number
  /** Total archived conversation entries. */
  readonly count: number
  /** Timestamp (ms) of the last archived entry, when any. */
  readonly lastTs?: number
  /** Preview of the last archived entry body. */
  readonly lastBody?: string
  /** The peer is signalling "busy"/typing right now. */
  readonly typing?: boolean
  readonly addedAt?: string
}

export interface PendingRequest {
  /** Request id (= the request message id); what `friends.accept/reject` take. */
  readonly id: string
  readonly fp: Fingerprint
  /** Name on the requester's card. */
  readonly name: string
  /** Greeting text sent with the request. */
  readonly greeting: string
  readonly createdAt?: string
}

export interface InboundMessage {
  readonly id: A2AMessageId
  readonly from: Fingerprint
  /** Display name of the sender as known when the message arrived. */
  readonly name: string
  readonly body: string
  /** Unix epoch ms. */
  readonly ts: number
  /** 1-based line number in the peer's conversation archive, when archived. */
  readonly seq?: number
  readonly auto?: true
  /** Group provenance: the sender's human owner typed it, or their alter composed it. */
  readonly by?: 'owner' | 'alter'
  /** Which of the sender's seat agents composed a by=alter post (e.g. "DevBot"); absent = their default alter. */
  readonly agent?: string
  /** A2A message type (`text`, `app_share`, …). */
  readonly type?: string
  /** Absolute path of an attachment written to disk, when any. */
  readonly artifactPath?: string
  readonly artifactName?: string
}

export interface ConversationEntry {
  readonly seq: number
  readonly dir: 'in' | 'out'
  readonly id: A2AMessageId
  readonly body: string
  readonly ts: number
  /** Sender fingerprint — set on group entries (who spoke); absent on pairwise threads. */
  readonly from?: string
  readonly type?: string
  readonly auto?: true
  /** Group provenance (wire spec §14.7): who produced the message — the human owner or their alter. */
  readonly by?: 'owner' | 'alter'
  /** Which of the sender's seat agents composed a by=alter post; absent = their default alter. */
  readonly agent?: string
  /** Outbound only: `sent` | `queued` | `error`. */
  readonly status?: string
  readonly artifactName?: string
}

/**
 * Governance layer of one group (wire spec §14.7; camelCase mirror of the Go
 * `a2a.GroupProfile`, wire keys are snake_case). A missing profile (pre-§14.7
 * groups) means "everything allowed, chat room, invite-only".
 */
export interface GroupProfile {
  /** Preset this profile started from (display only): standard | announcement | agents | tasks | casual. */
  readonly template?: string
  /** Room module rendering this group; "" or "chat" = the built-in chat room. */
  readonly room?: string
  /** May humans (by=owner) post. */
  readonly speakHumans: boolean
  /** May alters (by=alter) post. */
  readonly speakAgents: boolean
  /** Which MEMBERS may post at all ("" = all). */
  readonly speakWho?: 'all' | 'owner' | 'admins'
  /** Join policy ("" = invite). */
  readonly join?: 'invite' | 'apply' | 'open' | 'paid'
  /** Paid-join price in USDC decimal ("1.00"); required when join = paid. */
  readonly joinPrice?: string
  /** Paid-join receiving address (0x, Base); required when join = paid. */
  readonly joinAddr?: string
  /** Payment instruction shown to applicants (display only). */
  readonly joinNote?: string
  /** When a member's alter wakes on group traffic ("" = mention). */
  readonly agentWake?: 'mention' | 'always' | 'never'
  /** Default reply tier of alters in this group ("" = draft). */
  readonly agentTier?: 'notify' | 'draft' | 'auto'
  /** Cap on one alter's automatic posts per hour (0 = default 10). */
  readonly autoPerHour?: number
  /** Cap on consecutive alter-only exchanges (0 = default 3). */
  readonly agentRounds?: number
  /** Member fingerprints with invite/kick/pin rights (the owner always has them). */
  readonly admins?: readonly string[]
  /** List the group's card on its relay so strangers can find it and apply. */
  readonly public?: boolean
  readonly tags?: readonly string[]
  /** Free-text group constitution (markdown), injected into member alters' prompts. */
  readonly rules?: string
}

/** One pinned message on the group home (owner/admin curated; not part of the chat stream). */
export interface GroupPin {
  readonly id: string
  readonly from: Fingerprint
  readonly ts: number
  readonly body: string
}

/** One pending application of a stranger to join the group (owner only). */
export interface JoinPayment {
  readonly tx_hash: string
  readonly amount: string
  readonly to: string
  /** The 0x address the applicant paid from (identity binding: the owner's
   * node rejects proofs whose on-chain sender differs from this). */
  readonly payer?: string
  /** Wallet-secret receipt (from the applicant's local paygate) proving the
   * applicant controls `payer` - without it the owner cannot cryptographically
   * tie the wallet to the applicant. */
  readonly proof?: JoinPaymentProof
  readonly note?: string
}

/** The wire form of a wallet-secret receipt (paygate POST /v2/pay/join.receipt). */
export interface JoinPaymentProof {
  readonly message: string
  readonly pubkey: string
  readonly sig: string
}

export interface GroupApplication {
  readonly fp: Fingerprint
  readonly name: string
  readonly note: string
  readonly ts?: number
  /** Paid-join proof when the group's join policy is "paid". */
  readonly payment?: JoinPayment
}

/** A group's PUBLIC card (from `group.lookup`): what a stranger sees before applying. */
export interface GroupCardView {
  readonly gid: string
  readonly name: string
  readonly join: string
  readonly members: number
  readonly joinPrice?: string
  readonly joinAddr?: string
  readonly joinNote?: string
  /** First ~280 chars of the group rules (may carry the paid-join marker). */
  readonly rulesHead?: string
}

/** One group I am in (sender-key fan-out group, wire spec §14). */
export interface Group {
  readonly gid: string
  readonly name: string
  readonly ownerFp: Fingerprint
  /** I am the owner (can kick; cannot leave). */
  readonly mine: boolean
  /** Roster version (bumps on membership changes). */
  readonly version: number
  /** Member count (including me). */
  readonly members: number
  readonly unread: number
  readonly count: number
  readonly lastTs?: number
  readonly lastBody?: string
  /** Governance profile (§14.7); absent on legacy groups. */
  readonly profile?: GroupProfile
}

export interface GroupMember {
  readonly fp: Fingerprint
  readonly name: string
  /** Seat-agent names this member announced in the group (their @-able agents). */
  readonly agents?: readonly string[]
}

export interface GroupInfo extends Group {
  readonly memberList: readonly GroupMember[]
  /** Pinned messages, oldest first. */
  readonly pins: readonly GroupPin[]
  /** My role in this group. */
  readonly myRole: 'owner' | 'admin' | 'member'
  /** Pending join applications (present for the owner only). */
  readonly applications?: readonly GroupApplication[]
}

/** Optional parts of one send. */
export interface SendOptions {
  /** Absolute path of one local file to attach. */
  readonly file?: string
  /** Mark the message as the alter's automatic reply (A2A `auto` flag; the receiver's loop guard). */
  readonly auto?: boolean
}

export interface SendReceipt {
  readonly id: A2AMessageId
  readonly seq?: number
  /** `sent`, or `queued` when the relay was unreachable (outbox retry). */
  readonly status: string
}

export type BackendState = 'starting' | 'ready' | 'restarting' | 'stopped' | 'error'

export interface BackendStatus {
  readonly backend: BackendKind
  readonly state: BackendState
  readonly pid?: number
  readonly restarts: number
  readonly lastError?: string
  readonly relay?: string
  readonly home?: string
  readonly protocol?: string
  readonly version?: string
  /** The `soulnet` executable the backend spawned (soulnet backend only). */
  readonly binary?: string
  /** How it was found: `setting` | `platform-package` | `path` | `plugin-bin`. */
  readonly binarySource?: string
}

export type NetworkEvent =
  | { readonly kind: 'message'; readonly message: InboundMessage }
  | { readonly kind: 'typing'; readonly fp: Fingerprint; readonly on: boolean }
  | { readonly kind: 'friend_request'; readonly request: PendingRequest }
  | { readonly kind: 'friend_accept'; readonly friend: Friend }
  | { readonly kind: 'presence'; readonly fp: Fingerprint; readonly online: boolean }
  | { readonly kind: 'status'; readonly status: BackendStatus }
  | { readonly kind: 'group_message'; readonly gid: string; readonly message: InboundMessage }
  /** A member's seat is working in the group (`agent` = which of their agents, absent = their alter); presence metadata. */
  | { readonly kind: 'group_typing'; readonly gid: string; readonly fp: Fingerprint; readonly agent?: string; readonly on: boolean }
  /** Joined / roster changed / left one group — refetch the group list. */
  | { readonly kind: 'group_update'; readonly gid: string }
  /** A stranger applied to join one of my groups (owner side); `request.payment` is the paid-join proof when the group's join policy is "paid". */
  | { readonly kind: 'group_application'; readonly gid: string; readonly request: { readonly fp: Fingerprint; readonly name: string; readonly note: string; readonly payment?: JoinPayment } }

/** Error raised by a backend call; `code` follows the soulnet JSON-RPC table (cmd/soulnet/README.md). */
export class NetworkError extends Error {
  override readonly name = 'NetworkError'
  constructor(message: string, readonly code: number, readonly data?: unknown) {
    super(message)
  }
}

/** The capability profile (a2a/profile.json) as seen by the plugin. Extra fields
 * (skills, contexts, …) pass through untouched — the peer signs what it gets. */
export interface CapabilityProfile {
  v?: number
  fingerprint?: string
  tags?: readonly string[]
  summary?: string
  intro?: string
  accepting?: boolean
  updated_at?: string
  sig?: string
  /** Public USDC (Base) wallet address; lets other agents pay this alter. */
  usdc_address?: string
  [key: string]: unknown
}

/** A directory hit: the published card plus an optional signed profile. */
export interface DirectoryHit {
  readonly profile?: CapabilityProfile
  [key: string]: unknown
}

/** soulnet application error codes (branch on these, never on message text). */
export const NetworkErrorCode = {
  noIdentity: -32001,
  notFriend: -32002,
  identityExists: -32003,
  notFound: -32004,
  badCard: -32005,
  network: -32006,
  badFile: -32007,
  noProfile: -32008,
  /** Plugin-side: the backend process is not running / could not be reached. */
  peerUnavailable: -32099,
} as const

export interface NetworkClient {
  readonly backend: BackendKind
  status(): BackendStatus
  /** `undefined` until an identity was created (first run). */
  identity(): Promise<Identity | undefined>
  createIdentity(name: string): Promise<Identity>
  /** Sign an A2A request (`method`+`path`+`ts`, relay VerifyRequest bytes) with the identity's private key. The key never leaves the backend — local services (e.g. the payment gateway) verify it with the public key from identity.json. No identity → NetworkErrorCode.noIdentity. */
  signRequest(method: string, path: string, ts: string): Promise<string>
  /** Own card URI (`soulmirror://card?...`). */
  card(): Promise<string>
  parseCard(uri: string): Promise<{ fp: Fingerprint; name: string; uri: string }>
  readonly friends: {
    list(): Promise<readonly Friend[]>
    pending(): Promise<readonly PendingRequest[]>
    /** Send a friend request from a card URI; `note` is both the local note and the greeting. */
    add(cardUri: string, note?: string): Promise<Friend>
    accept(requestId: string, note?: string): Promise<Friend>
    reject(requestId: string): Promise<void>
    /** Change the local note and/or the per-friend protocol override (`protocol: ''` clears it). */
    set(fp: Fingerprint, patch: { remark?: string; protocol?: string }): Promise<Friend>
    remove(fp: Fingerprint): Promise<void>
    /** A friend's card URI (from the card snapshot), e.g. to forward it. Not a friend → NetworkErrorCode.notFriend. */
    card(fp: Fingerprint): Promise<{ fp: Fingerprint; name: string; uri: string }>
  }
  /** Capability profile (published to the directory): `usdc_address` is how other agents find this alter's wallet. */
  readonly profile: {
    /** The local capability profile, or undefined when none was saved yet. */
    get(): Promise<CapabilityProfile | undefined>
    /** Save + sign the profile locally (does NOT publish). */
    save(profile: CapabilityProfile): Promise<CapabilityProfile>
  }
  /** The opt-in capability directory (relay side): how agents find each other's wallet addresses. */
  readonly directory: {
    /** Fetch one entry (card + signed profile) by fingerprint. null when absent. */
    fetch(fp: Fingerprint): Promise<DirectoryHit | null>
    /** Publish the saved profile (signed) to the directory. */
    publish(profile?: CapabilityProfile): Promise<{ ok: boolean; published: boolean }>
  }
  readonly groups: {
    list(): Promise<readonly Group[]>
    /** Create a group with me as owner plus the given FRIEND fingerprints; `profile` = the governance layer (default: the standard template). */
    create(name: string, members: readonly Fingerprint[], profile?: GroupProfile): Promise<GroupInfo>
    info(gid: string): Promise<GroupInfo>
    /** Encrypt once, upload once; the relay fans out to the other members. `by` = provenance (default owner); `agent` names the seat agent behind a by=alter post. */
    send(gid: string, body: string, options?: { by?: 'owner' | 'alter'; auto?: boolean; agent?: string }): Promise<SendReceipt>
    conversation(gid: string, options?: { since?: number; limit?: number }): Promise<{ entries: readonly ConversationEntry[] }>
    markRead(gid: string, seq: number): Promise<void>
    /** Announce this seat's enabled agent names in the group (member-list metadata for @-autocomplete; empty clears). */
    announceVoices(gid: string, voices: readonly string[]): Promise<void>
    /** "This seat is working here" signal (agent = which of my seat agents, '' = the alter); presence metadata, never archived. */
    typing(gid: string, on: boolean, agent?: string): Promise<void>
    /** Leave (members only; the owner cannot leave its own group). */
    leave(gid: string): Promise<void>
    /** Remove a member (owner only; triggers a rekey on every remaining member). */
    kick(gid: string, fp: Fingerprint): Promise<void>
    /** Replace the group's governance profile (owner; admins may only pin/invite). */
    setProfile(gid: string, profile: GroupProfile): Promise<void>
    /** Pin a message on the group home (owner/admin). */
    pin(gid: string, body: string): Promise<void>
    /** Remove one pin by id (owner/admin). */
    unpin(gid: string, id: string): Promise<void>
    /** Fetch a group's PUBLIC card (join policy, paid-join price/address) from its URI. */
    lookup(uri: string): Promise<GroupCardView | null>
    /** Apply to join a group from its public URI; `payment` is the paid-join proof for join policy "paid". */
    apply(uri: string, note?: string, payment?: JoinPayment): Promise<{ gid: string }>
    /** Pending join applications (owner only). */
    applications(gid: string): Promise<readonly GroupApplication[]>
    /** Approve one application: the applicant joins the roster (owner). */
    approve(gid: string, fp: Fingerprint): Promise<void>
    /** Reject one application (owner). */
    applicationReject(gid: string, fp: Fingerprint): Promise<void>
    /** Invite a FRIEND into the group (owner/admin). */
    invite(gid: string, fp: Fingerprint): Promise<void>
  }
  send(to: Fingerprint, body: string, options?: SendOptions): Promise<SendReceipt>
  typing(to: Fingerprint, on: boolean): Promise<void>
  conversation(fp: Fingerprint, options?: { since?: number; limit?: number }): Promise<{ entries: readonly ConversationEntry[]; typing: boolean }>
  markRead(fp: Fingerprint, seq: number): Promise<void>
  presence(fps: readonly Fingerprint[]): Promise<Record<string, boolean>>
  subscribe(listener: (event: NetworkEvent) => void): () => void
  /** Stop the backend (clean `shutdown` for the peer process). Idempotent. */
  dispose(): Promise<void>
  /**
   * Ask the backend to restart the HOST process after a self-upgrade: the
   * peer spawns a detached helper that waits for this host (and the old peer)
   * to die, then execs the command again. Present only on backends whose peer
   * offers `host.relaunch` (soulnet >= 0.2); absent = the UI falls back to
   * "restart dsh manually".
   */
  relaunch?(params: { pid: number; exec: string; argv: readonly string[]; cwd: string }): Promise<void>
  /** Test/dev seam: make the backend deliver an inbound message now. Absent on real backends. */
  readonly debug?: {
    inject(from: Fingerprint, body: string): void
  }
}
