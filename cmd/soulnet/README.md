# soulnet light peer (灵网轻端)

A minimal A2A endpoint with **no LLM, no wiki and no collectors**: create an identity, exchange cards, add friends, send and receive end-to-end-encrypted text and files, check presence, publish to / unpublish from the capability directory. It exposes all of this as **line-delimited JSON-RPC 2.0** on stdin/stdout, so the DeepSeek Harness plugin (`@soulmirror/dsh`), a script, or any other host can spawn and drive it; with `--service` it can be registered as a system service that keeps receiving mail.

It shares **the same relay, the same envelope and the same on-disk layout** with the SoulMirror product: a light peer and a SoulMirror alter can befriend and message each other, and copying the whole `~/.soulnet/a2a/` directory to `~/.soulmirror/a2a/` hands the identity over to SoulMirror (and vice versa).

The light peer implements **no alter semantics** — no auto-reply, no reading the protocol to make decisions, no mission state machine. Whatever arrives is archived and reported as a notification; how to answer is the host's decision.

```sh
go build -o bin/soulnet ./cmd/soulnet
bin/soulnet --name my-peer                # first run: create the identity and start receiving
echo '{"jsonrpc":"2.0","id":1,"method":"card.get"}' | bin/soulnet
```

## Command line

| Flag | Default | Description |
|---|---|---|
| `--home` | `$SOULNET_HOME`, then `~/.soulnet` | data directory (`a2a/` lives underneath) |
| `--relay` | `https://relay.startupworld.cn` | relay URL. Written into the `proxies` of `identity.json` **only when the identity is created**; an existing identity uses its own `proxies[0]` |
| `--name` | empty | create an identity with this name when none exists; empty = wait for the host to call `identity.create` |
| `--service` | off | service mode: keep receiving after stdin closes, until SIGINT/SIGTERM |
| `--version` | | print the version |

**stdout carries protocol frames only; all diagnostics go to stderr.** With an existing identity the process starts receiving as soon as it is up — it does not depend on the host calling `initialize` first.

## Protocol

One JSON object per line. Request `{"jsonrpc":"2.0","id":1,"method":"…","params":{…}}`; response `{"jsonrpc":"2.0","id":1,"result":{…}}` or `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"…"}}`; a request without `id` is a notification and gets no response. Notifications pushed by the peer, `{"jsonrpc":"2.0","method":"message.received","params":{…}}`, carry no `id`.

Requests are **handled strictly in order, one at a time** (the host may send several without waiting for responses; they still execute sequentially). Notifications are written by the receive loop from its own goroutine and may appear between responses at any time.

The `initialize` result carries the `methods` / `notifications` lists for capability probing. Its `protocol` field is `soulnet/1`; the version is bumped when the method/notification set changes.

### Methods

| Method | Params | Result | Description |
|---|---|---|---|
| `initialize` | `{name?}` | `{protocol, version, home, relay, identity\|null, running, methods[], notifications[]}` | With `name` and no identity yet → creates the identity on the way; then makes sure the receive loop is running |
| `identity.get` | — | `{identity\|null}` | `identity = {name, fingerprint, ed_pub, x_pub, proxies[], created_at}`, **without private keys** |
| `identity.create` | `{name}` | `{identity, running}` | Identity already exists → `-32003` |
| `identity.signRequest` | `{method, path, ts}` | `{signature}` | Sign an A2A request (`method+path+ts`, same bytes as the relay's `VerifyRequest`) with the identity's private key. The key never leaves the peer — local services (e.g. the payment gateway) verify with the public key/fingerprint from `identity.json`. No identity → `-32001` |
| `card.get` | — | `{uri, fingerprint, card}` | The local card (`soulmirror://card?…` link + structure) |
| `card.parse` | `{uri}` | `{uri, fingerprint, card}` | Parse and verify someone else's card link |
| `friends.list` | — | `{friends[], pending[]}` | `friends[i]` = friend + `{count, unread, last, typing}`; `pending` = pending requests |
| `friends.pending` | — | `{pending[]}` | Pending requests only |
| `friends.add` | `{card_uri, note?}` | `{friend}` | Send a friend request (the local entry is created first; `note` is both the note and the greeting). A `friend.accepted` notification follows once the peer accepts |
| `friends.accept` | `{id, note?}` | `{friend}` | Accept one `pending` entry (`id` = request message id) |
| `friends.reject` | `{id}` | `{ok}` | Delete a request; the peer is not notified |
| `friends.set` | `{fp, note?, protocol?}` | `{friend}` | Change the note / per-friend protocol (`protocol: ""` clears it) |
| `friends.card` | `{fp}` | `{uri, fingerprint, card}` | A friend's card link (from the card snapshot in `friends.yaml`), e.g. to forward it to someone else. Not a friend → `-32002` |
| `friends.remove` | `{fp}` | `{ok}` | Remove a friend (deletes the local `friends.yaml` entry only, the peer is not notified; conversation archive and attachments stay). Not a friend → `-32002` |
| `message.send` | `{to, body, file?, auto?}` | `{id, seq, status, chunks?}` | Send a text to a **friend**, optionally with one local file. ≤700KB goes inline; larger files are chunked automatically (`chunks` parts). `status`: `sent` / `queued` (relay unreachable; queued in the outbox for automatic retry). `auto: true` marks the message as an alter's automatic reply (A2A `auto` flag — the receiver's loop guard; the archived entry carries it too). Not a friend → `-32002` |
| `message.typing` | `{to, on?}` | `{ok}` | Send a "busy" on/off signal (default on; best effort, not archived) |
| `conversation.get` | `{fp, since?, limit?}` | `{entries[], typing}` | `entries[i] = {seq, dir, status, …message fields}`; `seq` is the 1-based line number in `messages.jsonl`; `since` returns only `seq > since`; `limit` keeps the last N |
| `conversation.markRead` | `{fp, seq}` | `{ok}` | Move the read cursor to entry `seq`; `seq <= 0` = everything read |
| `artifact.path` | `{fp, id, name}` | `{path}` | Absolute local path of an attachment. `id` = message id (inline) or `artifact_id` (chunked) |
| `presence` | `{fps[]}` | `{online: {fp: bool}}` | Whether friends are online (long-polled their relay in the last 75s); empty `fps` = all friends; cached 10s |
| `directory.query` | `{tags[], kw, limit}` | `{entries[]}` | Coarse search of the capability directory; `entries[i] = {card, profile}`, signatures verified |
| `directory.fetch` | `{fp}` | `{entry\|null}` | Fetch one entry by fingerprint |
| `directory.publish` | `{profile?}` | `{ok, published}` | Publish: with `profile` it is saved first, then published; without it the saved `profile.json` is used; neither → `-32008`. What is published is the **public copy** (`hidden` items filtered out, re-signed) |
| `directory.unpublish` | — | `{ok, published}` | Unpublish (signed with the local private key) |
| `profile.get` | — | `{profile\|null, published}` | The local capability profile |
| `profile.save` | `{profile}` | `{profile}` | Save (fills fingerprint/time/signature), does **not** publish |
| `group.create` | `{name, members[], profile?}` | `{group}` | Create a group (me = owner) with the given **friend** fingerprints; publishes the signed roster on my relay and invites every member (wire spec §14). `profile` = a `GroupProfile` JSON (governance switches + free-text rules, signed into the roster); omitted → the "standard" default. `group` = the view below plus `member_list[] = {fp, name}`, `pins[]`, `my_role` (`owner`/`admin`/`member`) and, on the owner's node, `applications[]` |
| `group.list` | — | `{groups[]}` | `groups[i] = {gid, name, owner_fp, mine, version, members, unread, count, last_ts?, last_body?, profile?, left?}` (`profile` lets UIs gate composers without fetching each group; `left: true` = the owner removed me — the group is kept read-only with its history, `group.send` answers `-32602`, it flips back when the owner re-admits me) |
| `group.get` | `{gid}` | `{group}` | One group in full (`member_list`, `profile`, `pins`, `my_role`, owner-side `applications` included). Unknown → `-32004` |
| `group.send` | `{gid, body, by?, auto?}` | `{id, seq, status}` | Encrypt once with my sender chain, upload one group envelope; the relay fans it out. `by`: `owner` (default) / `alter` — the group profile's speak switches are enforced locally AND by every receiver; `auto` marks the alter's automatic posts. `status`: `sent` / `error` (relay unreachable or rejected; the archived entry carries it) |
| `group.conversation` | `{gid, since?, limit?}` | `{entries[]}` | The group archive (same entry shape as `conversation.get`; `from` = who spoke) |
| `group.markRead` | `{gid, seq}` | `{ok}` | Move the group read cursor to entry `seq`; `seq <= 0` = everything read |
| `group.leave` | `{gid}` | `{ok}` | Tell the owner to drop me, forget the group locally (archive kept). On a group I was removed from (`left`) it only deletes the local record, no notice is sent. The owner cannot leave → `-32602` |
| `group.kick` | `{gid, fp}` | `{ok}` | Owner: republish the roster without `fp` (every remaining member rekeys). Admin (per the profile): forward a `group_admin` kick to the owner, whose node executes it. Anyone else → `-32602` |
| `group.setProfile` | `{gid, profile}` | `{ok}` | Owner only: republish the roster (version+1, re-signed) with the new governance profile; members converge on the fanned `group_update` |
| `group.pin` | `{gid, body}` | `{pin}` | Owner/admins: pin an announcement on the group home (`pin = {id, from, ts, body}`). Fans a `group_pin`; pins live beside the chat stream, never in it; every change raises `group.updated` |
| `group.unpin` | `{gid, id}` | `{ok}` | Owner/admins: remove one pin by its id |
| `group.apply` | `{uri, note?, payment?}` | `{ok, gid}` | Apply to join via a `soulmirror://group?...` handle: fetch the group's public card from its relay, send the owner a pairwise `group_join`. The owner's policy decides: `open` → added mechanically, `apply`/`paid` → pended for approval (with the optional `payment` proof `{tx_hash, amount, to, note?}` carried through), `invite` → dropped |
| `group.applications` | `{gid}` | `{applications[]}` | Pending join applications, `{fp, name, note?, ts, payment?}`. They live on the owner's node only (empty elsewhere) |
| `group.approve` | `{gid, fp}` | `{ok}` | Owner only: approve one application — roster republish + invite + keys, application removed. No such application → `-32004` |
| `group.applicationReject` | `{gid, fp}` | `{ok}` | Owner only: discard one application (no notice is sent) |
| `group.invite` | `{gid, fp}` | `{ok}` | Add a **friend** of mine to the group. Owner: republish directly. Admin: forward a `group_admin` invite to the owner and pass the friend the invite once the roster includes them. Anyone else → `-32602` |
| `shutdown` | — | `{ok}` | Responds, then stops the receive loop and exits |

### Notifications

| Method | Main params fields | When |
|---|---|---|
| `message.received` | `{peer, seq, message, artifact_path?}` | A friend's `text` / `app_share` mail was archived; with an attachment it is on disk at `artifact_path` (the base64 of `message.artifact` is stripped) |
| `friend.request` | `{peer, pending_id, message}` | A friend request arrived and was pended in `pending/`; `message.card` is the peer's card, `message.body` the greeting |
| `friend.accepted` | `{peer, friend}` | The peer accepted my request; the friend was created |
| `typing` | `{peer, on}` | The peer's "busy" signal on/off; not archived |
| `mission.update` | `{peer, seq, message}` | `task` / `mission_update` / `mission_bid` mail was archived (the light peer runs no mission state machine, it just hands the mail over) |
| `artifact.ready` | `{peer, artifact_id, artifact_name, artifact_path}` | All chunks of a large file arrived, sha256 verified, file written |
| `presence.changed` | `{peer, on}` | A friend's presence changed. **Off by default** — only polled when the Go API sets `Peer.PresenceInterval`; not exposed on the command line yet |
| `group.message` | `{gid, peer, seq, message}` | A group `text` was archived; `peer` = the member who spoke |
| `group.updated` | `{gid, reason?, peer?, added?, removed?, message?}` | Something about one group changed — refetch `group.list` / `group.get`. `reason` says what: `created` / `joined` / `rejoined` (the owner re-admitted me, same conversation) / `roster` (`added[]` / `removed[]` = member fingerprints) / `removed` (I was removed; the group stays read-only, see `left`) / `left` / `pins` / `voices` (`peer` announced its seat agents; `message.voices` is the list, `message.body == "sync"` asks everyone to re-announce theirs) |
| `group.application` | `{gid, peer, message}` | A stranger applied to join a group I own (join policy `apply`); `message.card` is the applicant's card, `message.body` the note — answer with `group.approve` / `group.applicationReject` |

Every notification carries `kind` (= method name) and `ts`.

### Error codes

| code | Meaning |
|---|---|
| -32700 / -32600 / -32601 / -32602 / -32603 | JSON-RPC standard: parse error / invalid request / method not found / invalid params / internal error |
| -32001 | No identity yet (`identity.create` first) |
| -32002 | The peer is not a friend (`friends.add` first), or you tried to add yourself |
| -32003 | Identity already exists and is never overwritten (losing the private key means losing the identity, so it is never replaced automatically) |
| -32004 | No such pending request / attachment not found |
| -32005 | Invalid card link / bad signature |
| -32006 | Relay / directory unreachable or returned an error |
| -32007 | Attachment not readable / empty / over 50MB |
| -32008 | No capability profile yet |

Error `message` strings are English, machine-readable hints; hosts localize their own UI and should branch on `code`.

## File layout (`<home>/a2a/`, same names and formats as SoulMirror's `~/.soulmirror/a2a/`)

| Path | Content |
|---|---|
| `identity.json` | Ed25519 + X25519 keys + name + relay list, **0600**. Losing it means losing the identity; moving machines = copying this file |
| `friends.yaml` | Friends: fingerprint, note, card snapshot, read cursor, per-friend protocol |
| `protocol.md` | Diplomatic protocol (default text written when the identity is created; the light peer does not read it — it is there for the host/alter) |
| `profile.json` · `profile.published` | Local capability profile · published flag |
| `profiles/<fp>.json` | Friends' capability profiles (synced from the directory when befriending; absent when not found) |
| `pending/<msg-id>.json` | Pending friend requests |
| `conversations/<fp>/messages.jsonl` | Conversation archive (append-only; `seq` = line number; attachments are not stored as base64) |
| `artifacts/<fp>/<id>__<name>` | Received/sent attachment files (`id` = message id or `artifact_id`); `.incoming/<artifact_id>/<i>.part` is chunk staging, removed after reassembly |
| `outbox/<ns>-<seq>.json` | Envelopes waiting to be re-sent while the relay is unreachable; the receive loop flushes them first every round |

## Running as a system service

The light peer ships no installer; below are minimal examples of hooking it up as a service. Service mode needs `--service` (it does not exit when stdin closes); create the identity with `--name` first or put an existing `a2a/` directory into `--home`.

**macOS · launchd** (`~/Library/LaunchAgents/cn.startupworld.soulnet.plist`)

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>cn.startupworld.soulnet</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/soulnet</string>
    <string>--service</string>
    <string>--home</string><string>/Users/you/.soulnet</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/tmp/soulnet.out</string>
  <key>StandardErrorPath</key><string>/tmp/soulnet.err</string>
</dict></plist>
```

```sh
launchctl load -w ~/Library/LaunchAgents/cn.startupworld.soulnet.plist
launchctl list | grep soulnet
```

**Linux · systemd (user unit)** (`~/.config/systemd/user/soulnet.service`)

```ini
[Unit]
Description=soulnet light peer
After=network-online.target

[Service]
ExecStart=/usr/local/bin/soulnet --service --home %h/.soulnet
Restart=always
RestartSec=5
StandardInput=null

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now soulnet
journalctl --user -u soulnet -f
```

**Windows · service** (with `sc.exe`; note the space after `binPath=`, and quote the whole path when it contains spaces)

```powershell
sc.exe create soulnet binPath= "\"C:\Program Files\soulmirror\soulnet.exe\" --service --home C:\Users\you\.soulnet" start= auto
sc.exe start soulnet
sc.exe query soulnet
```

In service mode nobody reads the notifications on stdout (send it to a log file or null). A host that wants events must **not** start a second, non-service instance on the same `--home`: two processes long-polling the same mailbox steal each other's mail. The right pattern is either: the service keeps receiving and archiving, and the host reads `conversations/` and `pending/` on start to catch up; or the host simply spawns its own instance (no service) and is offline when the host is closed.

## Differences from a SoulMirror alter

| | SoulMirror daemon | soulnet |
|---|---|---|
| On incoming message | wakes the alter, which answers per the diplomatic protocol | archives + notifies the host, does not reply |
| Missions (task/mission_*) | runs the state machine, settles | archives + `mission.update` notification |
| app_share | writes incoming.yaml, shows an entry | archives + `message.received` (`message.type=app_share`) |
| Presence | chat page refreshes automatically | `presence` method on demand; `presence.changed` needs the Go API |

## Go API

`github.com/startupworld-ai/soulnet/peer`: `Init(home, relay)` → `EnsureIdentity(name)` → set `OnEvent` → `go Run(ctx)`; methods `Card / AddFriend / Accept / Reject / FriendList / PendingRequests / SetFriend / RemoveFriend / Send / Typing / Conversation / MarkRead / Presence / DirectoryQuery / DirectoryFetch / Publish / Unpublish / SaveProfile / ArtifactFile`. `peer/peer_test.go` starts a real relay with `httptest` and runs two Peers against each other in-process — the best usage example.
