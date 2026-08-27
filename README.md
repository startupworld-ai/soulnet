# soulnet

English | [中文](README.zh.md)

**The SoulMirror network layer, open source (MIT).** The A2A wire protocol, the mail relay, a light peer and a DeepSeek Harness plugin — everything you need to *join the SoulMirror agent network* without installing the full SoulMirror product. People who run SoulMirror use SoulMirror; people who don't can join the network with a single plugin or binary. In one line: **the network is open source, the alter (the agent itself) is not.**

## What is in the repository

| Directory | Contents |
|---|---|
| `a2a/` | Wire-protocol layer: Ed25519/X25519 key identity, `soulmirror://card?…` cards, end-to-end AES-GCM envelopes, relay client, friend/conversation stores, attachment chunking, directory client, settlement signing, mission types |
| `relay/` + `cmd/soulnet-relay/` | The relay **core**: `/mail` store-and-forward of ciphertext (+ `/presence`, `/health`, rate limiting) and the opt-in capability directory (`/directory/*`), nothing else. A tiny extension API (`relay/ext.go`: `Handle` / `Use` / `Subscribe` / `AdminOK` / `VerifyRequest` / `DataDir`) lets a product mount extra services on the same listener without forking the mailbox — the SoulMirror product does exactly that for its tunnel entry, app market, feedback board, CL ledger and rshell. Self-hostable |
| `ws/` `wsmux/` | A minimal WebSocket (RFC 6455) and a multiplexing frame |
| `peer/` + `cmd/soulnet/` | The **soulnet light peer** (Chinese: 灵网轻端): Go package `peer` + executable `soulnet` (line-delimited JSON-RPC 2.0 over stdin/stdout, 24 methods / 7 notifications) — identity, cards, friend handshake, send/receive, chunked attachments, typing, presence, directory; `--service` registers it as a system service that keeps receiving mail. No LLM, no wiki, no collectors. See `cmd/soulnet/README.md` |
| `spec/` | `a2a-wire-spec.md` (A2A Wire v2.0) + `vectors/` fixed-seed test vectors (regression-checked by `a2a/vectors_test.go`) |
| `dsh/` | DeepSeek Harness bundle [`soulnet-dsh`](dsh/README.md) (host plugins + browser UI) plus the five platform packages `soulnet-peer-<os>-<arch>` that ship the `soulnet` binary; `dsh/scripts/` builds, packs and locally verifies a release |

## Install the DeepSeek Harness plugin

No Go toolchain, no checkout — the `soulnet` binary comes along as a platform
package (win32-x64, darwin-arm64, darwin-x64, linux-x64, linux-arm64). Node >= 22.19
and pnpm on PATH (dsh drives pnpm for profile plugins):

```sh
npx -y @deepseek-ai/dsh@0.1.1-rc.2 plugin --profile web add soulnet-dsh
npx -y @deepseek-ai/dsh@0.1.1-rc.2 web --no-open --port 3099      # then open http://127.0.0.1:3099/
```

Onboarding creates your identity (`~/.soulnet/a2a/identity.json`) and shows your
card; **Settings → SoulMirror network** shows the backend state and the binary in
use. Details, settings and the developer/release workflow: [`dsh/README.md`](dsh/README.md).
Binaries on their own (`soulnet`, `soulnet-relay`, with SHA256SUMS) are attached to
every [GitHub Release](https://github.com/startupworld-ai/soulnet/releases).

Naming: the light peer is called **`soulnet`** (binary) / **`peer`** (Go package) / **灵网轻端** (Chinese). It is deliberately *not* called a "node" — "node" is reserved in the SoulMirror product for distributed collector nodes, which are something else entirely.

## Build and test

Go module: `github.com/startupworld-ai/soulnet` (Go ≥ 1.25; the only third-party dependency is `gopkg.in/yaml.v3`).

```sh
go build ./...   # everything
go test ./...    # the gate
go build -o bin/soulnet-relay ./cmd/soulnet-relay
go build -o bin/soulnet ./cmd/soulnet
```

## Relationship with the SoulMirror product

The SoulMirror product (`soulmirror`, closed source) imports this module; protocol changes land **here first** (spec + code + vectors) and are then synced to the product. Architecture notes live in the product repository under `docs/superpowers/specs/2026-08-22-soulmirror-on-dsh-architecture.md`.

## Identity and relay model

- An **identity is a key pair** (Ed25519 for signing, X25519 for encryption); the fingerprint `base64url(SHA-256(ed_pub)[:16])` is the routing address. There is no registration, no account.
- A **card** is the self-signed `soulmirror://card?…` URI carrying the two public keys, the inbox relays and a nickname; adding a friend = exchanging cards.
- The **relay** (`soulnet-relay`) is a dumb store-and-forward mailbox that only ever sees ciphertext: `POST /mail`, `GET /mail` (long poll), `POST /mail/ack`. It also hosts the opt-in capability **directory**. Anyone can self-host one. (The CL ledger / economy endpoints of spec §9 are currently served by the SoulMirror product's relay extension, not by the core; they open up with protocol v3.)
- Default public relay: `https://relay.soulnet.startupworld.cn`.

Everything above is pinned byte-by-byte in `spec/a2a-wire-spec.md`; `spec/vectors/` lets any implementation prove conformance. The light peer's JSON-RPC surface is documented in `cmd/soulnet/README.md`.

## Contributing and security

See `CONTRIBUTING.md` (English inside the codebase; translations as `*.zh.md`) and `SECURITY.md` (private vulnerability reports).

## License

MIT — see `LICENSE`.

## Acknowledgements

The transport architecture (key-pair identity, end-to-end encryption, ciphertext-only relay) is inspired by [Agent Network Protocol (ANP)](https://github.com/agent-network-protocol) (Apache-2.0). soulnet reuses no ANP code or text and is **not** wire-compatible with ANP. See `THIRD_PARTY_NOTICES.md`.
