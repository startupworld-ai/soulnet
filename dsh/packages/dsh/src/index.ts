/**
 * soulnet-dsh — host root entry = the `soulmirror-network` plugin.
 *
 * Provides `ctx.soulmirror` (NetworkClient: the `soulnet` light peer by
 * default, the in-memory fake on request) and `ctx.soulmirrorHome`, registers
 * the `soulmirror` user-settings namespace and mounts the browser-facing HTTP
 * API (./api). The bare package name is also what dsh's client-module scan keys
 * on, so this entry carries the browser bundle declaration (package.json
 * `dsh.client` + the `./client` export).
 *
 * Host side rule: NO @deepseek-ai VALUE imports into the harness instance
 * (types only; the one vendored library we do import, schemastery, is inlined).
 * A linked (`dsh plugin add ./packages/dsh`) package resolves bare specifiers
 * from its own real path, where the harness packages are not installed; and a
 * second copy of cordis/dsh-tools would be a different runtime instance anyway.
 */
import type { Context } from '@deepseek-ai/cordis'
import type { SettingsNamespace } from '@deepseek-ai/dsh-settings'
import { mountApi } from './api/index.ts'
import { createFakeNetworkClient } from './network/fake.ts'
import { createPaygateClient, type PaygateClient } from './network/paygate.ts'
import { paidJoinConfig } from './group-contract.ts'
import { createSoulnetNetworkClient, defaultSoulnetHome } from './network/soulnet.ts'
import type { NetworkClient } from './network/types.ts'
import { resolveSettings, SETTINGS_NAMESPACE, SOULMIRROR_SETTINGS_SCHEMA, type SoulmirrorSettings } from './settings.ts'

export type * from './network/types.ts'
export type * from './network/paygate.ts'
export type * from './events.ts'
export { SOULMIRROR_PLUGIN, RELAY_FORM } from './events.ts'
export { SETTINGS_NAMESPACE } from './settings.ts'
export type { SoulmirrorSettings } from './settings.ts'
// Per-group client settings (<home>/dsh-groups.json): host-side consumers (the
// sessions plugin's group routing) read the map through `readGroupSettings`.
export { GROUP_SETTINGS_FILE, GroupSettingsStore, groupSettingsPath, readGroupSettings } from './group-settings.ts'
export type { GroupSettings, GroupSettingsMap } from './group-settings.ts'

/** Live view of the `soulmirror` settings (the alter fields apply without a restart). */
export interface SoulmirrorConfig {
  current(): SoulmirrorSettings
}

declare module '@deepseek-ai/cordis' {
  interface Context {
    /** SoulMirror network client (identity / card / friends / send / subscribe). */
    soulmirror: NetworkClient
    /** Backend data directory (`a2a/` underneath: identity.json, friends.yaml, conversations/ …; same layout as ~/.soulmirror/a2a). */
    soulmirrorHome: string
    /** Live settings: `defaultTier` / `autoReplyPerHour` / `directSend` are read per use; connection fields apply on reload. */
    soulmirrorConfig: SoulmirrorConfig
    /** Local payment gateway (USDC via CDP): spawns the paygate process, signs A2A requests through the peer. Undefined when the backend is the fake. */
    soulmirrorPay: PaygateClient | undefined
  }
}

/** Composition entry config; every field is also a user setting (namespace `soulmirror`). */
export type Config = Partial<SoulmirrorSettings>

export const name = 'soulmirror-network'
export const inject: string[] = []

export function apply(ctx: Context, config: Config = {}): void {
  const log = (level: 'info' | 'warn' | 'error', message: string): void => {
    ctx.logger[level](`soulmirror-network: ${message}`)
  }

  // Settings: schema defaults < composition entry (`config`) < user document.
  // When the settings service is already composed we read the resolved value
  // now; otherwise we run on the entry config and register late for the UI.
  const entry = resolveSettings(config)
  let effective: SoulmirrorSettings = entry
  // `live` follows the user document: the connection fields still apply on
  // reload (the peer is already running), the alter fields are read per use.
  let live: SoulmirrorSettings = entry
  // Paygate-relevant fields: changing them restarts the gateway process so the
  // new CDP keys / proxy / network apply without reloading the plugin.
  const paygateFieldsChanged = (a: SoulmirrorSettings, b: SoulmirrorSettings): boolean =>
    a.cdpKeyId !== b.cdpKeyId || a.cdpKeySecret !== b.cdpKeySecret || a.cdpWalletSecret !== b.cdpWalletSecret
    || a.cdpNetwork !== b.cdpNetwork || a.paygateProxy !== b.paygateProxy || a.paygatePort !== b.paygatePort || a.paygateBinary !== b.paygateBinary
  const settingsNow = ctx.get('settings')
  // paygateApplied snapshots the settings the gateway was last spawned with;
  // syncPaygate restarts the gateway when they differ — covering BOTH the
  // "settings service loads after the gateway was spawned" case (ctx.inject
  // branch) and later user edits.
  let paygateApplied: SoulmirrorSettings | undefined
  const syncPaygate = (): void => {
    if (paygate === undefined) return
    if (paygateApplied === undefined) { paygateApplied = live; return }
    if (!paygateFieldsChanged(paygateApplied, live)) return
    paygateApplied = live
    log('info', 'paygate settings changed; restarting the payment gateway')
    paygate.restart()
  }
  if (settingsNow !== undefined) {
    const scope = settingsNow.register(SETTINGS_NAMESPACE as SettingsNamespace, SOULMIRROR_SETTINGS_SCHEMA, { base: config, applies: 'restart' })
    effective = resolveSettings(scope.get() as Partial<SoulmirrorSettings>)
    live = effective
    scope.watch(() => {
      live = resolveSettings(scope.get() as Partial<SoulmirrorSettings>)
      log('info', `settings changed (tier=${live.defaultTier}, autoReplyPerHour=${live.autoReplyPerHour}, directSend=${String(live.directSend)} apply now; connection fields apply when the plugin reloads)`)
      syncPaygate()
    })
  } else {
    ctx.inject(['settings'], (sctx) => {
      const scope = sctx.settings.register(SETTINGS_NAMESPACE as SettingsNamespace, SOULMIRROR_SETTINGS_SCHEMA, { base: config, applies: 'restart' })
      live = resolveSettings(scope.get() as Partial<SoulmirrorSettings>)
      syncPaygate() // settings loaded after the gateway was spawned → respawn with them
      scope.watch(() => {
        live = resolveSettings(scope.get() as Partial<SoulmirrorSettings>)
        log('info', 'settings changed; alter fields apply now, connection fields when the plugin reloads')
        syncPaygate()
      })
    })
  }
  const liveConfig: SoulmirrorConfig = { current: () => live }
  ctx.provide('soulmirrorConfig', liveConfig)

  const home = effective.home !== '' ? effective.home : defaultSoulnetHome()
  let client: NetworkClient
  if (effective.backend === 'fake') {
    client = createFakeNetworkClient()
  } else {
    const peer = createSoulnetNetworkClient({
      home,
      relay: effective.relay,
      displayName: effective.displayName,
      ...(effective.peerBinary === '' ? {} : { peerBinary: effective.peerBinary }),
      logger: log,
    })
    peer.start()
    client = peer
  }

  ctx.provide('soulmirrorHome', home)
  ctx.provide('soulmirror', client)
  ctx.effect(() => () => {
    void client.dispose().catch((error: unknown) => { log('warn', `dispose failed: ${String(error)}`) })
  }, 'soulmirror-network: backend process')

  // Local payment gateway (USDC via CDP). Spawned alongside the peer; the fake
  // backend cannot sign A2A requests, so it is skipped there.
  let paygate: PaygateClient | undefined
  if (effective.backend !== 'fake') {
    paygate = createPaygateClient({
      home,
      net: client,
      settings: () => ({
        paygateBinary: live.paygateBinary,
        paygateProxy: live.paygateProxy,
        paygatePort: live.paygatePort,
        cdpKeyId: live.cdpKeyId,
        cdpKeySecret: live.cdpKeySecret,
        cdpWalletSecret: live.cdpWalletSecret,
        cdpNetwork: live.cdpNetwork,
      }),
      logger: (message) => log('info', message),
    })
    paygate.start()
    paygateApplied = { ...live } // baseline: what this spawn used
    ctx.effect(() => () => {
      void paygate?.dispose().catch((error: unknown) => { log('warn', `paygate dispose failed: ${String(error)}`) })
    }, 'soulmirror-network: payment gateway')
  }
  ctx.provide('soulmirrorPay', paygate)

  // Paid group join: when an application carries a payment proof for a group
  // whose join policy is "paid", verify the transfer on-chain through the local
  // gateway (recipient == join_addr, amount >= join_price) and auto-approve.
  if (paygate !== undefined) {
    client.subscribe((event) => {
      if (event.kind !== 'group_application') return
      const req = event.request
      if (req.payment === undefined) return
      void (async () => {
        try {
          const group = await client.groups.info(event.gid)
          const profile = group.profile
          const paidConfig = paidJoinConfig(profile)
          if (paidConfig === undefined) return
          // Verify against the PUBLISHED price/address — never the applicant's
          // self-reported values, so underpaying (or paying a different address)
          // fails even though the applicant claims a matching proof. The payer
          // + wallet receipt bind the tx to the applicant (replay protection).
          const result = await paygate.call('POST', '/v2/pay/join.verify', {
            tx_hash: req.payment!.tx_hash,
            to: paidConfig.addr,
            amount: paidConfig.price,
            ...(req.payment!.payer === undefined ? {} : { payer: req.payment!.payer }),
            ...(req.payment!.proof === undefined ? {} : { proof: req.payment!.proof }),
          }) as { valid?: boolean; reason?: string }
          if (result.valid === true) {
            await client.groups.approve(event.gid, req.fp)
            log('info', `paid join verified & auto-approved: ${req.fp} → ${event.gid} (${req.payment!.amount} USDC)`)
            // Tell the group who just paid their way in (as the owner, so it
            // reads as an official notice). Best-effort.
            try {
              const who = req.name !== '' ? req.name : (req.fp.length > 10 ? `${req.fp.slice(0, 10)}…` : req.fp)
              await client.groups.send(event.gid, `✅ ${who} paid ${req.payment!.amount} USDC and joined the group`, { by: 'owner' })
            } catch (e) {
              log('warn', `paid join notice send failed: ${String(e)}`)
            }
          } else {
            log('warn', `paid join NOT verified (${result.reason ?? 'invalid'}): ${req.fp} → ${event.gid}`)
          }
        } catch (error: unknown) {
          log('warn', `paid join verification failed: ${String(error)}`)
        }
      })()
    })
  }

  mountApi(ctx, {
    client,
    home,
    settingsNamespace: SETTINGS_NAMESPACE,
    sessions: () => ctx.get('soulmirrorSessions'),
    settings: () => live,
    paygate: () => paygate,
    log,
  })
  log('info', `backend=${client.backend} home=${home} relay=${effective.relay}`)
}
