# dsh/ — SoulMirror for DeepSeek Harness (`soulnet-dsh`)

[中文](README.zh.md) | English

A DeepSeek Harness (dsh) **bundle**: install it into a dsh web profile and that
dsh becomes a node on the SoulMirror network — A2A identity, friends, encrypted
messaging — with **your alter** living in ONE ordinary dsh session ("My alter")
that speaks to every friend on your behalf. The chat UI is a set of dsh client
plugins (slots) — a SoulMirror page right of dsh's sidebar — not a replacement
of the dsh UI.

Status: **M1 — real backend + P2 sidebar entry + P4 conversation model: one
alter session, read-only friend threads, owner-reviewed drafts, dsh-styled
page**. The network backend is the `soulnet` light peer (Go, `../cmd/soulnet`,
`../peer`): the host plugin spawns the binary and drives it over stdio JSON-RPC
2.0 (`cmd/soulnet/README.md`, protocol `soulnet/1`). The in-memory fake from
the P0 spike is still selectable (`backend: fake`) for tests and UI work. The
spike's findings are kept as history in [SPIKE.md](SPIKE.md).

```
dsh/
├── pnpm-workspace.yaml          pnpm workspace (packages/*)
├── package.json                 workspace scripts: build / typecheck / test:unit / build:peer / pack:all / registry:local
├── tsconfig.base.json
├── SPIKE.md                     P0 findings (the 5 spike questions + install path) — history
├── spike-evidence/*.png, *.txt  screenshots / run logs backing SPIKE.md and every phase (p2-*, p2b-*, p3-*, p4-*, release-*)
├── scripts/
│   ├── build-peer-packages.mjs  go build (cross) soulnet into packages/soulnet-<os>-<arch>/bin/ [+ release assets]
│   ├── pack-all.mjs             pnpm pack the five platform packages + the plugin into dsh/dist/
│   ├── local-registry.mjs       throw-away npm registry serving dsh/dist/*.tgz (fresh-user install test)
│   └── fresh-user-check.mjs     the scripted proof against a running fresh dsh: ready + binary source + onboarding + a friend + 2 messages
├── packages/soulnet-<os>-<arch>/ five tiny npm packages (os/cpu fields, files: bin/) = soulnet-peer-windows-x64,
│                                -darwin-arm64, -darwin-x64, -linux-x64, -linux-arm64; bin/ is git-ignored and filled by the build script
└── packages/dsh/                the npm package soulnet-dsh
    ├── package.json             dsh.bundle (patch) + dsh.client (browser bundle) + exports + optionalDependencies on the five platform packages
    ├── README.md                the npm page (install / first run / where data lives)
    ├── cordis.patch.yml         the config layer: 4 host rows (network / sessions / tools / commands)
    ├── tsdown.config.ts         out-of-repo replica of dsh's client-bundle preset
    ├── tsconfig.json            host half (node types)      -> lib/types
    ├── tsconfig.client.json     browser half (react types)  -> lib/types/client
    ├── tsconfig.test.json       vitest sources (type check only)
    ├── vitest.config.ts
    ├── presets/soulmirror-chat/ agent preset copied into $DSH_HOME/.agent-presets on first run (fallback persona)
    ├── bin/                     (git-ignored) drop soulnet[.exe] here for a PATH-less dev install (last resort of the lookup)
    ├── test/                    vitest: jsonrpc, soulnet client (fake peer script), policy, alter-state,
    │                            drafts, tools-gate, friend-settings, inbox-state, page-state, integration
    └── src/
        ├── index.ts             host root = `soulmirror-network`: ctx.soulmirror (NetworkClient),
        │                        ctx.soulmirrorHome, ctx.soulmirrorConfig (live settings), `soulmirror`
        │                        settings namespace, browser API mount
        ├── settings.ts          schemastery schema of the `soulmirror` settings namespace
        ├── policy.ts            reply policy (pure): tiers, routeInbound (loop guard), sendGate (send now / draft), HourlyWindow
        ├── alter-state.ts       folds of the alter session log (pure): triggerOf (what woke the turn, with the friend),
        │                        latestFromEvents (the alter's latest state), chatFromEvents (the "My alter" transcript)
        ├── drafts.ts            <home>/a2a/dsh-pending.json — the alter's pending drafts the owner reviews
        ├── persona.ts           alter persona template + the prompt variables it references (roster, this turn's friend, drafts)
        ├── friend-settings.ts   <home>/a2a/dsh-friends.json (per-friend tier) + protocol.md helper
        ├── api/index.ts         browser-facing HTTP API + SSE on dsh's web server (/soulmirror/api/*)
        ├── events.ts            shared vocabulary: ids + the relay `user/message` source tag (+ plugin notes)
        ├── network/types.ts     NetworkClient interface (identity/card/friends/send/conversation/subscribe/...)
        ├── network/jsonrpc.ts   line-delimited JSON-RPC 2.0 endpoint over two streams
        ├── network/soulnet.ts   finds the binary (setting → platform package → PATH → plugin bin/), spawns `soulnet`, restarts with backoff, maps the wire
        ├── network/send.ts      send + archive read-back shared by the tool, approved drafts and the debug direct send
        ├── network/fake.ts      in-memory fake backend
        ├── sessions/index.ts    `soulmirror-sessions`: THE alter session (= the alter for every friend; no workspace of its own since P5) +
        │                        inbound routing per tier, owner instructions, persona, drafts (queue / approve /
        │                        reject / revise), alter state & transcript, migration of the P3 per-friend sessions
        ├── tools/index.ts       `soulmirror-tools`: 5 model tools (send gate: owner → send; auto tier → send flagged auto;
        │                        else → pending draft)
        ├── tools/define.ts      local typed defineTool helper over raw ToolDefinition
        ├── commands/index.ts    `soulmirror-commands`: /card /friends /add /soulmirror
        └── client/              browser bundle (lib/client.js): the SoulMirror page (SoulmirrorPage.tsx = shell.overlay;
                                 FriendList.tsx = middle column with the pinned "My alter" item; AlterPane.tsx = the
                                 alter chat + composer; FriendPane.tsx = read-only friend thread + action bar;
                                 DraftCard.tsx = approve / edit / revise / reject), page-state.ts + page-store.ts
                                 (pure folds, unit-tested), inbox-state.ts (friend rows + drafts folded from /state
                                 and SSE, unit-tested), SidebarEntry.tsx (sidebar.footer.action), InboxOverlay.tsx
                                 (new-mail toast), SidebarNavEntry.tsx (the `sidebar.nav.primary` row when soulnet-dsh-sidebar is installed),
                                 SettingsSection.tsx, Onboarding.tsx, alter-ui.tsx (tier select, friend settings,
                                 protocol editor), a2a-node.ts + A2ANode.tsx (bubbles in the native alter session),
                                 locales.ts (zh/en), styles.ts (one <style>, dsh --dsw-* tokens only)
```

## The conversation model (P4)

SoulMirror semantics, now literally in the UI: **the owner only ever talks to
their own alter**; the alter speaks to every friend on the owner's behalf;
**friend threads are read-only** for the owner (alter ↔ alter); what the alter
wants to send without the owner's word becomes a **draft the owner reviews**.

| Piece | Mechanism |
|---|---|
| **One alter session** | The sessions plugin creates ONE dsh session (title `My alter · SoulMirror`, preset `soulmirror-chat`, cwd = `<home>/a2a`; since P5 it is attached to no dsh workspace — it sits ungrouped in dsh's session list, the SoulMirror page is its home; the P2–P4 "SoulMirror" workspace is removed on start, sessions detached, nothing deleted) and persists its id in `<home>/a2a/dsh-sessions.json` (`{ alterSessionId }`); on restart it is resumed. Its agent has the `soulmirror_*` tools for ALL friends (`soulmirror_send_message {fingerprint, body}` etc.). Model = `ctx.agentDefaultModel.currentSelection()` at creation. |
| **"My alter" = the only composer** | The SoulMirror page's middle column pins **My alter** as the first item (never sorted away). Its right pane is a chat view of the alter session rendered by us from `GET /soulmirror/api/session.history` (`chatFromEvents`: owner messages, the alter's notes, relayed friend mail as cards, what it sent / queued, draft-decision notes, failed turns) + a composer → `POST alter.instruct {text}` (an owner `user/message` with `source: { kind: 'user' }` + `agent.followup()`). SSE `alter` frames refetch the transcript (debounced). **Open in dsh** jumps to the native session (trajectory / tools); its native composer works too. |
| **Friend items are read-only threads** | The A2A archive (`conversation.get` + SSE `message` / `outbound`): in = the friend (or their alter), out = my alter, with delivery state, day separators, load-older; a top banner "This is your alter's conversation with X — you can't speak here. Tell My alter."; **no composer**; a bottom ACTION BAR instead: drafts waiting · Card · Protocol & settings (tier + per-friend override) · "Talk to My alter →". Selecting a friend marks it read while the page is visible. |
| **Inbound mail → the alter session** | Every friend's mail is appended to the ALTER session as ONE relay `user/message` (`source: { kind:'plugin', plugin:'soulmirror', form:'relay', senderSessionId:<friend name>, a2a:{ id, fp, ts, auto?, type? } }`) carrying the friend context, and routed by that friend's tier (`routeInbound`): `notify` → append only; `draft` / `auto` → `agent.followup` wakes a turn. Loop guard: mail flagged `auto` and mail from a non-friend never wake a turn. The alter replies with `soulmirror_send_message` and the sender's fingerprint. |
| **Per-turn friend context** | The persona (`persona.ts`, agent-scoped `deployment:persona` section + prompt variables, registered in the agent factory's `setup` on create and resume) carries the owner's name, the **roster** (every friend: name · fingerprint · tier · protocol override), the global protocol, the pending drafts, and **this turn's friend** — resolved from the session log at assembly time (`triggerOf`): the friend whose mail woke the turn (+ its tier and override), or "none" for an owner instruction (the owner may name any friend; the roster has the fingerprints). |
| **Send gate → send now or draft** (`sendGate`) | Owner-initiated turn → **send now** (not flagged auto) to any friend. Mail from the target friend in the `auto` tier, under `autoReplyPerHour` → **send now, flagged `auto`** on the wire (`message.send {auto:true}`), counted per friend (`HourlyWindow`). Everything else → **`queueDraft`**: draft / notify tier, over the cap, a turn woken by an auto mail, answering friend A by writing to friend B, an unattributed trigger. The tool result says `outcome: 'draft-queued'` + `draftId`; the persona tells the alter to inform the owner in one line and not retry. **No dsh approval panel** in this path any more (it stays only as the fallback when the sessions face is absent, and for `soulmirror_add_friend`). |
| **Drafts are ours** | `<home>/a2a/dsh-pending.json` (`drafts.ts`): `{ id, fp, name, body, createdAt, reason, trigger, sessionId }`. The page renders a draft card (`DraftCard.tsx`, prototype `.wx-pending`) in the friend's read-only thread and in the My alter chat: **Send it** → the host sends through the peer as the alter (`auto:false`), archives it (SSE `outbound`) and removes the draft; **Edit** → a text box, then send the edited text; **Let the alter revise** → feedback box → the draft is discarded and the alter gets an owner instruction with the feedback (it rewrites and sends on the owner's word); **Reject** → discard. Every decision is also written into the alter session as a plugin NOTE (relay source + `a2a.note`, never a turn trigger) so the alter knows what happened. `POST drafts.decide {id, action, body?, feedback?}`, `GET drafts.list`; SSE `draft` frames (`added` / `removed`) keep every tab live; `/state` friend rows carry `drafts` (count) and the header shows the total. |
| **Middle column** (prototype #B) | Header (identity name, fingerprint, copy card, unread + drafts badges, protocol editor, refresh, close) · search · **My alter** pinned first (live dot, "n drafts waiting") · **New friends** (accept / reject) · **Friends** (avatar with presence dot and unread badge, tier pill, last preview + age — `[draft]` marker when a draft waits; unread-first then newest) · **Add friend** at the bottom. Left edge = right edge of dsh's sidebar (DOM measurement of our own footer button's column; ResizeObserver + poll during the collapse transition; 56 px on the rail). |
| **Style** | dsh tokens only (`--dsw-alias-*` / `--dsw-specific-*` set by ui-layout / ui-theme) + ui-primitives (`Button`, `Tooltip`, icons, `Toast`) in one injected `<style>`; layout and behaviour follow the SoulMirror prototype, colours and type follow dsh (the owner's call). |
| **Migration from P3** | The P3 per-friend sessions are retired: an old `dsh-sessions.json` `{ sessions: { fp: id } }` is kept as `legacyFriendSessions` (logged once, shown in Settings); those sessions stay in dsh's sidebar until deleted but receive no mail, and no new per-friend session is ever created. A tool call from such a session is gated like any other (by what woke that turn). |
| **Kept** | Unread badge on the sidebar footer entry + the new-mail toast (suppressed while the friend's thread is open on the page or the alter session is on screen), SSE live updates, Settings → SoulMirror network (protocol editor, default tier, per-hour cap, the debug **Send as myself** toggle — when on, a friend thread shows a small direct-send row that bypasses the alter; default off), slash commands (`/card`, `/friends` → popup opens the page on a friend, `/add`, `/soulmirror` → page / friend), zh / en, onboarding. The P2 "SoulMirror" `conversation.view` tab was dropped in P5 (the page + the sidebar entry are the way in). The P3 composer takeover of friend sessions is gone with the friend sessions. |

Host API (P4 changes): `POST alter.instruct {text}`; `GET session.latest` →
`{state: {sessionId, status, latest}}`; `GET session.history?limit=` →
`{sessionId, status, chat: {items, running, seq}}`; `GET drafts.list?fp=`;
`POST drafts.decide {id, action: approve|reject|revise, body?, feedback?}`;
`/state` → `drafts[]`, friend rows with `tier` / `tierExplicit` / `protocol` /
`drafts`, `alter: {sessionId, status, defaultTier, autoReplyPerHour, directSend,
protocolPath, protocolExists, legacyFriendSessions}`; SSE frames `alter`,
`outbound`, `draft`; `friends.open` and the `sessions` map are gone. **Host
changed → restart dsh** after updating.

Tests: `test/policy.test.ts` (tier routing, loop guard, send gate → send / draft,
rate window), `test/alter-state.test.ts` (trigger with the friend, the latest
fold with send outcomes, the transcript fold), `test/drafts.test.ts` (the
store), `test/tools-gate.test.ts` (the real tool against a fake context: owner
bypass to any friend, auto tier + cap + auto flag, draft tier → stored draft
and no approval, notify / loop guard / other friend / unknown → drafts, the
approval fallback without a sessions face), `test/inbox-state.test.ts` (draft
frames, draft counts per row), `test/page-state.test.ts` (the pinned My alter
selection), `test/friend-settings.test.ts`, `test/soulnet-client.test.ts`, Go
`cmd/soulnet/rpc_test.go`.

Real run (`spike-evidence/p4-real-run.txt`, `spike-evidence/p4-*.png`): dsh on
port 3096, isolated `DSH_HOME`, a local relay, two extra `soulnet` peers (Bob in
`draft`, Carol in `auto`) and — no model key on this machine — a **scripted
OpenAI-compatible server** on `llm-deepseek`'s `baseURL` that plays the alter
from the roster in the system prompt. Shown: instruct My alter → the alter
sends to Bob directly (`owner-initiated`) → Bob's read-only thread shows the
out bubble; Bob mails in `draft` → a pending card in Bob's thread (and the
`[draft]` marker / header count) → **Send it** delivers to Bob, **Reject**
sends nothing, **Edit** delivers the edited text, **Let the alter revise**
makes the alter rewrite and send on the owner's word; the My alter transcript
shows mail cards, send lines (sent / draft), the decision notes; Carol in
`auto` gets an auto reply flagged `auto: true`; an auto-flagged mail is
appended without a turn; a money question → no send, a note to the owner;
`notify` → appended, no turn; collapsed sidebar (left = 56); Open in dsh.

## Sidebar entry & inbox (P2)

The owner's first real test: mail arrived (logged, appended to the friend
session) while he was looking at the "SoulMirror" view tab of a *different*
session — which listed only that session's mail — and the friend session itself
was hidden in the sidebar (blank). P2 added one always-visible way in; since
P2b/P4 it toggles the SoulMirror page.

| Piece | Slot / mechanism |
|---|---|
| **Sidebar entry** | `sidebar.footer.action` (list, root; owner prop `{ wide }`) — a "SoulMirror" button stacked above Settings in both widths. Wide: icon + label + unread pill; collapsed rail: icon + red dot. The number is the unread sum over all friends. Clicking toggles the page. |
| **New-mail cue** | ui-primitives `Toast` "New message from <name>" (4 s) for mail whose thread is not open on the page and while the alter session is not the one on screen. The Toast primitive is `pointer-events: none` by design, so it is not clickable — open the mail from the badge / page. |
| **Live state** | `src/client/inbox-state.ts` folds the last `/soulmirror/api/state` answer with the SSE frames (`message` → unread+1 / preview / notice; `presence`, `typing`, `friend_accept`, `friend_request`, `outbound`, `draft`) so the badge and rows move instantly; the debounced refetch that follows a frame replaces the optimistic fold. `networkStore.markRead(fp)` zeroes locally and calls `conversation.markRead`. Pure module, unit-tested. |
| **Styling** | inline `--dsw-*` token styles plus one injected `<style>` — the client bundle has no CSS-module pipeline (SPIKE.md §6). |

Upstream asks this surfaced: dsh has **no slot near the New Session button** —
a `sidebar.top.action` (list) beside / under New Session would let the entry
sit where the eye goes first instead of the foot (see the roadmap list below).

## History: P2b page and P3 per-friend sessions

P2b (`spike-evidence/p2b-*.png`) built the page as friend list + chat pane with
a composer that sent **directly** to the friend; P3 (`spike-evidence/p3-real-run.txt`)
turned that composer into an instruction to the alter, but kept **one dsh
session per friend** (the alter for that friend), routed inbound mail per
tier with the `draft` tier going through **dsh's approval panel**, and showed an
"alter note" strip. The owner reviewed the UI and rejected that conversation
model: the owner must only ever talk to ONE alter, friend threads are
read-only, drafts are reviewed on the page. P4 (this) is that correction; the
per-friend sessions, the friend-session composer takeover, the alter strip and
the approval-panel path are gone. Low-level mechanisms that survived: the relay
`user/message` (no custom session events, SPIKE.md §1), the trigger-from-the-log
send gate, the agent-scoped persona with live variables, the tiers + rate limit
+ loop guard, the `auto` flag on the wire, the peer's `friends.card` /
`friends.set {protocol}`.

## Rooms: how to write a room plugin

A group has three layers (wire spec §14.7): **transport** (sender-key fan-out),
**governance** (the `GroupProfile` — machine-enforced switches + free-text
rules), and the **room** — the client application that renders the group. The
room is a **keyed client slot**, `'group.room'`, declared by soulnet-dsh's page
registration; the built-in chat is just its first occupant (key `chat`,
`src/client/rooms/ChatRoom.tsx`), registered through the same public slot API
any other dsh plugin would use. The GroupPane is the room HOST: it renders the
header and the group home itself and dispatches the body on `profile.room`
(missing/unknown key → the chat room with a small notice).

A third-party room plugin's client half:

```ts
import type { ClientContext } from '@deepseek-ai/dsh-client-runtime/client'
// Type-only: merges the 'group.room' SlotMap row and brings RoomOwnerProps.
import type { RoomOwnerProps } from 'soulnet-dsh/client'

function KanbanRoom({ gid, group, me, members, thread, actions, canSpeakHuman, canSpeakAgent }: RoomOwnerProps) {
  // render the group your way; post with actions.send(body, { by: 'owner' })
  return null
}

export const inject = ['slots']
export function apply(ctx: ClientContext): void {
  ctx.slots.inject('group.room', () => ctx.slots.register({ name: 'group.room', key: 'kanban' }, KanbanRoom))
}
```

The owner props are the whole contract (`RoomOwnerProps` in
`src/client/group-room.ts`): the group row with its `profile`, `me`, the
member roster, the visible archive slice (`thread`), the `actions`
(`send(body, {by})` / `loadOlder()` / `markRead()`), and the resolved speak
gate (`canSpeakHuman` / `canSpeakAgent`). A group opts into a room by carrying
`room: "kanban"` in its profile (`group.setProfile`); nodes without that room
installed fall back to chat, so a room must degrade to its message stream
gracefully. Everything else stays the host's job — governance enforcement,
the home panel, membership. The chat room's composer also owns the per-group
"my alter participates" switch (`<home>/dsh-groups.json`, API route
`group.settings`; host-side consumers read it with `readGroupSettings` from
`soulnet-dsh`).

## Install

### As a user (one line; published on npm)

No Go toolchain, no checkout. Node >= 22.19 and pnpm on PATH (`npm i -g pnpm`;
dsh drives pnpm for profile plugins):

```sh
npx -y @deepseek-ai/dsh@0.1.1-rc.2 plugin --profile web add soulnet-dsh
npx -y @deepseek-ai/dsh@0.1.1-rc.2 web --no-open --port 3099     # open http://127.0.0.1:3099/
```

`dsh plugin add` is `pnpm add` inside `$DSH_HOME/profiles/web` (hoisted node
linker), so the plugin lands in `profiles/web/node_modules/soulnet-dsh`
and pnpm also installs the ONE optional dependency that matches the machine,
`soulnet-peer-<os>-<arch>` (os/cpu fields; the other four are
skipped), as a sibling: `profiles/web/node_modules/soulnet-peer-windows-x64/bin/soulnet.exe`
and so on. Onboarding in the browser (or **Settings → SoulMirror network**)
creates the identity; the same section shows **binary: <path> (from the
platform package)** once the backend is `running`. Remove with
`dsh plugin --profile web remove soulnet-dsh`.

**How the binary is found** (`src/network/soulnet.ts`, `locateSoulnetBinary`),
first hit wins, the winner and its source are in `BackendStatus.binary` /
`binarySource` (`/soulmirror/api/state` → `status`, Settings, the host log line
`soulnet binary: … (platform-package)`):

1. the `peerBinary` setting (absolute path, or a bare name looked up on PATH) — `setting`;
2. `soulnet-peer-<os>-<arch>` (`<os>` = `process.platform`, except `win32` -> `windows`) resolved with
   `require.resolve('<pkg>/package.json')` from `lib/index.js` and from its realpath
   (works for pnpm's hoisted profile layout and for the isolated virtual store) →
   `<pkg>/bin/soulnet[.exe]` — `platform-package` (on POSIX the mode bit is repaired if
   the tarball was packed on Windows);
3. `soulnet` on PATH — `path`;
4. `<plugin dir>/bin/soulnet[.exe]` — `plugin-bin` (dev: drop a binary there).

No hit → backend state `error` with a message naming the platform package, the
setting, PATH and bin/. Unsupported `os-arch` pairs (anything but the five)
need 1, 3 or 4.

### Dev loop (from a checkout)

```sh
cd dsh
pnpm install
pnpm run build:peer:current     # go build soulnet into packages/soulnet-<host>/bin/ (all five: pnpm run build:peer)
pnpm run build                  # tsc (types, both halves) + tsdown -> packages/dsh/lib/
pnpm run typecheck              # no emit (host, client, tests)

export DSH_HOME=/tmp/dsh-home   # PowerShell: $env:DSH_HOME = "C:\tmp\dsh-home"   (keep the real ~/.dsh untouched)
npx -y @deepseek-ai/dsh@0.1.1-rc.2 plugin --profile web add ./packages/dsh     # link the checkout
npx -y @deepseek-ai/dsh@0.1.1-rc.2 web --no-open --port 3099
```

The workspace links the five platform packages into `packages/dsh/node_modules/@startupworld-ai/`,
so after `build:peer:current` the linked checkout resolves the binary through
the platform-package step exactly like an installed copy (no PATH, no `bin/`).
Rebuild → refresh the browser; host changes → restart dsh.

### Verify a release locally (fresh-user test, no npm publish)

What a stranger's install will do, reproduced from tarballs: pack all six
packages, serve them from a throw-away local registry (it proxies everything
else to npmjs), and let `dsh plugin add` fetch the plugin BY NAME so pnpm
resolves the optional platform package the same way it will against the real
registry. Only the registry URL differs from the published one-liner.

```powershell
# 0. from the repo root, once: the five binaries, the plugin, all tarballs into dsh/dist/
node dsh/scripts/build-peer-packages.mjs          # needs Go (or build only the host: --current)
cd dsh; pnpm install; pnpm run build; pnpm run pack:all

# 1. the throw-away registry (keep it running in this shell)
pnpm run registry:local                            # http://127.0.0.1:4873 serving dsh/dist/*.tgz

# 2. in a second shell: a clean dsh home, install BY NAME from the local registry (the flag is forwarded to pnpm;
#    pnpm 11 ignores npm_config_registry, so pass it as an argument)
$env:DSH_HOME = "$env:TEMP\dsh-fresh-home"          # sh: export DSH_HOME=/tmp/dsh-fresh-home
npx -y @deepseek-ai/dsh@0.1.1-rc.2 plugin --profile web add soulnet-dsh --registry http://127.0.0.1:4873
Get-ChildItem "$env:DSH_HOME\profiles\web\node_modules" -Filter "soulnet*"   # soulnet-dsh + soulnet-peer-<this os-arch> only
$env:SOULNET_HOME = "$env:TEMP\dsh-fresh-soulnet"   # keep ~/.soulnet untouched; a local relay instead of the public one:
@"
- id: soulmirror-network
  config:
    relay: http://127.0.0.1:9395
"@ | Set-Content "$env:DSH_HOME\profiles\web\cordis.patch.yml"
npx -y @deepseek-ai/dsh@0.1.1-rc.2 web --no-open --port 3095

# 3. prove it (third shell, repo root): backend ready + binary from the platform package, onboarding through the API,
#    a second peer (the repo's bin/soulnet) befriends it on the local relay, one message each way
..\bin\soulnet-relay.exe --addr 127.0.0.1:9395 --data "$env:TEMP\dsh-fresh-relay"        # started before dsh web, kept running
curl http://127.0.0.1:3095/soulmirror/api/state               # status.state == "ready", status.binarySource == "platform-package"
node scripts/fresh-user-check.mjs --dsh http://127.0.0.1:3095 --relay http://127.0.0.1:9395 --soulnet ..\bin\soulnet.exe --name Fresh
```

`fresh-user-check.mjs` exits 0 only when every check passed; its log and the
install transcript are `spike-evidence/release-fresh-user.txt` /
`release-fresh-install.txt`, the Settings section showing **binary: …
soulnet-win32-x64\bin\soulnet.exe (from the platform package) · soulnet 0.1.0**
is `spike-evidence/release-settings-binary.png`. Tear-down: stop dsh, the relay
and the registry, delete `$env:TEMP\dsh-fresh-*` — nothing under `~/.dsh` or
`~/.soulnet` is touched.

### Release (the owner)

Versions live in `packages/dsh/package.json` AND the five
`packages/soulnet-*/package.json` (all equal; the plugin's `optionalDependencies`
are `workspace:*`, rewritten to the real version by `pnpm pack` / `pnpm publish`).
`.github/workflows/release.yml` runs on a `v*` tag (or by hand as a dry run):
go test, cross-build the five binaries + `soulnet` / `soulnet-relay` release
assets with SHA256SUMS, build + typecheck + unit tests, `pnpm publish --access public`
the five platform packages and then the plugin, and a GitHub Release with the
binaries attached. It needs the repository secret `NPM_TOKEN` (an automation
token with publish rights on the `@startupworld-ai` npm org; `npm access` the
six names to the org once they exist) and, optionally, the repository variable
`NPM_PROVENANCE=true` (npm provenance; requires the public repo to match
`repository.url`). Bump every version, commit, `git tag v0.1.0`, push the tag.

## Prerequisites (from source)

- Node >= 22.19 (dsh's own engine range), pnpm >= 10 (`npm i -g pnpm`).
- Go 1.25+ to build the `soulnet` light peer (and a local relay for the integration test). Users of the published package do not need Go.
- A dsh runtime. Either is fine:
  - **npx** (first run downloads ~ minutes): `npx -y @deepseek-ai/dsh@0.1.1-rc.2 --version`
  - **source checkout** of deepseek-harness: `pnpm install && pnpm run build`, then `pnpm dsh …` instead of `dsh …` below.
- dsh packages are published under the `next` dist-tag; `latest` points at stale
  `0.0.1-rc.x` builds. Every `@deepseek-ai/*` devDependency here is pinned to
  `0.1.1-rc.2` on purpose — bump them together with the dsh version you target.

## Build

```sh
# the peer binary, into the platform packages (repo root; all five targets, or --current for the host)
node dsh/scripts/build-peer-packages.mjs
# or by hand for the integration test / a second peer:
go build -o bin/soulnet ./cmd/soulnet            # Windows: -o bin/soulnet.exe
go build -o bin/soulnet-relay ./cmd/soulnet-relay

cd dsh
pnpm install
pnpm run build          # tsc (types, both halves) + tsdown -> packages/dsh/lib/
pnpm run typecheck      # no emit (host, client, tests)
pnpm run pack:all       # six tarballs into dsh/dist/ (needs the binaries)
```

`lib/` is git-ignored. Outputs: `lib/index.js` (network plugin, root entry),
`lib/sessions.js`, `lib/tools.js`, `lib/commands.js` (ESM, node), `lib/client.js`
(browser bundle, lazy-CJS factory for the dsh module loader), `lib/types/**/*.d.ts`.

Why the build looks the way it does (the two rules you must keep):

1. **Host half has zero `@deepseek-ai/*` value imports into the harness instance**
   (types only). A linked package resolves bare specifiers from its own real path
   where the harness is not installed, and a second copy of cordis/dsh-tools
   would be a different runtime instance anyway. Tools are raw `ToolDefinition`
   objects (typed through the local `src/tools/define.ts`), messages are built by
   hand, home paths are resolved by hand. The one vendored library we do import
   (`@deepseek-ai/schemastery`, for the settings schema) is inlined into `lib/index.js`.
2. **Browser half is one CJS file wrapped as**
   `window.__ModuleLoader__.load({ id, factory: (require) => { …; return module.exports } })`.
   Only the dsh baseline is `require`d (react, react/jsx-runtime, react-dom,
   @deepseek-ai/cordis, dsh-client-ui-slots, dsh-client-ui-primitives,
   dsh-client-runtime/client) plus anything listed in `dsh.client.external`;
   everything else is inlined; any other `@deepseek-ai/*` value import fails the
   build (purity gate replicated in `tsdown.config.ts`). Cross-plugin
   collaboration goes through cordis services (`ctx.slots`, `ctx.locale`,
   `ctx.conversationEvents`, `ctx.sessions`, `ctx.settingsScope`, `ctx.commandUi`).

## Tests

```sh
cd dsh/packages/dsh
pnpm test:unit           # vitest: JSON-RPC, soulnet client (+ binary lookup), policy, alter-state, drafts, tools gate, inbox / page state
pnpm test:integration    # real soulnet binary + local soulnet-relay (two identities, two homes)
pnpm test                # both
```

The integration test (`test/integration.test.ts`) needs, at the repo root,
`bin/soulnet[.exe]` and `bin/soulnet-relay[.exe]`:

```sh
go build -o bin/soulnet ./cmd/soulnet
go build -o bin/soulnet-relay ./cmd/soulnet-relay
```

(or point `SOULNET_BIN` / `SOULNET_RELAY_BIN` at them). It starts the relay on
a free loopback port with a temp data dir, spawns two peers (Alice, Bob) with
temp homes, and asserts: identities created, A→B friend request arrives as a
`friend_request` event, B accepts → A gets `friend_accept`, A sends a text → B's
client emits `message` and the archive / unread / read cursor agree, presence
answers, a non-friend send fails with code -32002. It is **skipped** when a
binary is missing; `SOULNET_INTEGRATION=1` makes that a failure,
`SOULNET_INTEGRATION_VERBOSE=1` prints the relay/peer logs. Never point it at
the public relay.

## Using it

Open `http://127.0.0.1:3099/` (or whatever port you started dsh on):

- First run: the onboarding step asks for a display name and creates the
  identity (key pair in `<home>/a2a/identity.json`), shows the card URI with a
  Copy button and the backup warning. Or do it in **Settings → SoulMirror network**.
- Click **SoulMirror** at the sidebar foot: the page opens on **My alter**. Tell
  it what to do ("tell Bob I am in at 3") — it writes to the friend through
  `soulmirror_send_message`; the friend's thread (left) shows the out bubble.
- Add a friend: paste a card URI under **Add friend** at the bottom of the list
  (or `/add <card_uri>`), or accept under **New friends**. A friend item is the
  read-only conversation between your alter and theirs.
- Mail from a friend in the `draft` tier (the default) wakes your alter; what
  it wants to reply appears as a **draft card** in that friend's thread and in
  My alter — Send it / Edit / Let the alter revise / Reject. `auto` sends by
  itself (rate-limited, flagged `auto`); `notify` only shows the mail.
- `/card` prints your card URI; `/friends` lists friends + pending (bare
  invocation: popup that opens the page on a friend); `/soulmirror` opens the page.
- With a model key the `soulmirror_*` tools are callable from any session; a
  send from any session on the user's word goes out directly, anything else is
  queued as a draft.

Per-profile overrides go into `$DSH_HOME/profiles/web/cordis.patch.yml` (a
patch replaces the whole `config` of a row); the same fields are user settings:

```yaml
- id: soulmirror-network
  config:
    backend: soulnet            # soulnet | fake
    relay: http://127.0.0.1:9390   # a local relay for testing; default https://relay.startupworld.cn
    home: C:\tmp\soulnet-home      # default $SOULNET_HOME, then ~/.soulnet
    # displayName: Alice          # create the identity on first start without onboarding
    # peerBinary: C:\tools\soulnet.exe   # overrides the platform package / PATH / bin lookup
```

## Where things go at runtime

- `<home>/a2a/` (default `~/.soulnet/a2a/`) — identity.json (0600), friends.yaml,
  conversations/, pending/, outbox/ … (same layout as `~/.soulmirror/a2a/`); the
  dsh workspace path; `dsh-sessions.json` = `{ alterSessionId, legacyFriendSessions? }`;
  `dsh-friends.json` = per-friend reply tier; `dsh-pending.json` = the alter's
  pending drafts; `protocol.md` = the diplomacy protocol; `dsh-sessions.log` is
  the plugin's own log (inbound mail, drafts, decisions, resume refusals).
- `$DSH_HOME/.agent-presets/soulmirror-chat/` — the agent preset, copied on
  first run from `packages/dsh/presets/`.
- `$DSH_HOME/sessions/<cwd-slug>/<session-id>/session.jsonl.zstd` — dsh's own
  session logs (Node 22: `zlib.zstdDecompressSync`, multi-frame).
- `$DSH_HOME/profiles/web/settings.yaml` — the user layer of the `soulmirror` settings namespace.

## Roadmap (from the architecture plan)

P0 spike → **M1 network & identity** → **P2 UI: sidebar entry, unread badge,
global inbox, new-mail toast** → P2b chat surface → P3 talking through your
alter → **P4 the SoulMirror conversation model: one alter session ("My alter"
pinned first, the only composer), read-only friend threads with the action bar,
owner-reviewed drafts (send / edit / revise / reject) instead of the approval
panel, per-turn friend context in the persona, dsh-styled page (this)** →
next: shadow the "Context injection" row in the native alter session,
attachments in the composer, task cards / status pills in friend threads,
per-friend auto-reply counters in the UI, `/protocol` command, a first-turn
nudge so the alter session is never blank in dsh's own list. Open upstream asks
to dsh: plugin session-event registration / `ignorable` on append; a sidebar
badge or non-blank marking; a `sidebar.top.action` slot near the New Session
button; a pinned-session API; the blank-session row label should honour a
projected title; a client-side Remote registration surface for out-of-repo
plugins.
