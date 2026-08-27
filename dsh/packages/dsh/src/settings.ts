/**
 * The `soulmirror` user-settings namespace (dsh `ctx.settings`): what the
 * browser settings section "SoulMirror network" edits and what the network
 * plugin reads when it spawns the backend. The connection fields apply on
 * the next plugin (re)load — the peer process is spawned with them
 * (`applies: 'restart'`); the alter fields (`defaultTier`, `autoReplyPerHour`,
 * `directSend`) are read live through `ctx.soulmirrorConfig.current()`. The
 * tiers decide how the alter handles a friend's mail (P4: `draft` = the
 * alter's reply waits as a pending draft on the SoulMirror page).
 *
 * `@deepseek-ai/schemastery` is a VALUE import here on purpose: the settings
 * seam needs a real schemastery schema (callable validator + `toJSON()` for
 * the browser form). It is a vendored, dependency-free library and gets
 * inlined into lib/index.js by tsdown, so the host half still has zero
 * `@deepseek-ai/*` runtime edges into the harness instance (see ../README.md).
 */
import z from '@deepseek-ai/schemastery'
import { DEFAULT_RELAY } from './network/soulnet.ts'
import type { BackendKind } from './network/types.ts'
import { DEFAULT_AUTO_REPLY_PER_HOUR, DEFAULT_REPLY_TIER, normalizeTier, type ReplyTier } from './policy.ts'

export const SETTINGS_NAMESPACE = 'soulmirror'

/** How much capability the alter session gets. `comms` = the SoulMirror-only preset; `full` = dsh's standard preset (shell / filesystem, like a normal dsh session). */
export type AlterMode = 'comms' | 'full'

export interface SoulmirrorSettings {
  /** Relay (mail office) URL; baked into identity.json when the identity is created. */
  relay: string
  /** Display name used when the identity is created on first start; empty = onboarding asks. */
  displayName: string
  backend: BackendKind
  /** Path of the `soulnet` binary; empty = PATH, then <plugin dir>/bin/. */
  peerBinary: string
  /** Data directory (`--home`); empty = $SOULNET_HOME, then ~/.soulnet. */
  home: string
  /** Reply tier for friends without their own setting. */
  defaultTier: ReplyTier
  /** Cap on automatic replies per friend per hour in the `auto` tier. */
  autoReplyPerHour: number
  /** Debug: offer "Send as myself" in the friend pane (bypasses the alter). */
  directSend: boolean
  /** Payment gateway: binary path (empty = `paygate` on PATH). */
  paygateBinary: string
  /** Payment gateway: loopback port. */
  paygatePort: number
  /** Proxy for the payment gateway's CDP calls (e.g. http://127.0.0.1:10808); empty = no proxy. */
  paygateProxy: string
  /** CDP API key id (empty = no CDP → tier-1 manual-address mode). */
  cdpKeyId: string
  /** CDP API key secret (base64 Ed25519 or PEM EC P-256). */
  cdpKeySecret: string
  /** CDP wallet secret (base64 DER EC P-256 PKCS8). */
  cdpWalletSecret: string
  /** CDP network. */
  cdpNetwork: 'base-sepolia' | 'base'
  /** Capability mode of the alter session (applies to the next alter session created/resumed). */
  alterMode: AlterMode
}

export const DEFAULT_PAYGATE_PORT = 9001

export const SOULMIRROR_SETTINGS_SCHEMA = z.object({
  relay: z.string().default(DEFAULT_RELAY).description('Relay URL (used when the identity is created).'),
  displayName: z.string().default('').description('Display name for a new identity (first start only).'),
  backend: z.union([z.const('soulnet'), z.const('fake')]).default('soulnet').description('soulnet = the light peer binary; fake = in-memory test backend.'),
  peerBinary: z.string().default('').description('Path of the soulnet binary; empty = PATH, then the plugin bin/ directory.'),
  home: z.string().default('').description('Data directory; empty = $SOULNET_HOME, then ~/.soulnet.'),
  defaultTier: z.union([z.const('notify'), z.const('draft'), z.const('auto')]).default(DEFAULT_REPLY_TIER).description('Default reply tier for friends: notify = mail is only shown; draft = the alter drafts a reply you review on the SoulMirror page; auto = the alter replies by itself (rate-limited).'),
  autoReplyPerHour: z.number().default(DEFAULT_AUTO_REPLY_PER_HOUR).description('Maximum automatic replies per friend per hour in the auto tier (0 disables).'),
  directSend: z.boolean().default(false).description('Debug: offer "Send as myself" in a friend thread (bypasses the alter); off by default.'),
  paygateBinary: z.string().default('').description('Path of the paygate binary (USDC payment gateway); empty = `paygate` on PATH.'),
  paygatePort: z.number().default(DEFAULT_PAYGATE_PORT).description('Loopback port of the local payment gateway (default 9001).'),
  paygateProxy: z.string().default('').description('Proxy for the gateway\'s Coinbase CDP calls, e.g. http://127.0.0.1:10808. Empty = direct (needed when your network blocks Coinbase).'),
  cdpKeyId: z.string().default('').description('CDP API key ID (portal.cdp.coinbase.com/api-keys/secret). Empty = no CDP (manual-address mode: receive + pay manually).'),
  cdpKeySecret: z.string().default('').description('CDP API key secret (from the downloaded cdp_api_key.json `privateKey`).'),
  cdpWalletSecret: z.string().default('').description('CDP wallet secret (portal.cdp.coinbase.com/wallets/non-custodial/security; shown exactly once).'),
  cdpNetwork: z.union([z.const('base-sepolia'), z.const('base')]).default('base-sepolia').description('CDP network: base-sepolia = testnet (recommended while testing), base = mainnet (real funds).'),
  alterMode: z.union([z.const('comms'), z.const('full')]).default('comms').description('Alter capability: comms = SoulMirror-only preset (messages/groups); full = dsh standard preset (shell + filesystem, like a normal dsh session). Applies to the next alter session.')
})

/** Fill in defaults for a partial section (plugin config or a stored user section). */
export function resolveSettings(partial: Partial<SoulmirrorSettings> | undefined): SoulmirrorSettings {
  const perHour = typeof partial?.autoReplyPerHour === 'number' && Number.isFinite(partial.autoReplyPerHour)
    ? Math.max(0, Math.floor(partial.autoReplyPerHour))
    : DEFAULT_AUTO_REPLY_PER_HOUR
  const port = typeof partial?.paygatePort === 'number' && Number.isFinite(partial.paygatePort) && partial.paygatePort > 0
    ? Math.floor(partial.paygatePort)
    : DEFAULT_PAYGATE_PORT
  return {
    relay: partial?.relay !== undefined && partial.relay.trim() !== '' ? partial.relay.trim() : DEFAULT_RELAY,
    displayName: partial?.displayName ?? '',
    backend: partial?.backend === 'fake' ? 'fake' : 'soulnet',
    peerBinary: partial?.peerBinary ?? '',
    home: partial?.home ?? '',
    defaultTier: normalizeTier(partial?.defaultTier),
    autoReplyPerHour: perHour,
    directSend: partial?.directSend === true,
    paygateBinary: partial?.paygateBinary ?? '',
    paygatePort: port,
    paygateProxy: partial?.paygateProxy ?? '',
    cdpKeyId: partial?.cdpKeyId ?? '',
    cdpKeySecret: partial?.cdpKeySecret ?? '',
    cdpWalletSecret: partial?.cdpWalletSecret ?? '',
    cdpNetwork: partial?.cdpNetwork === 'base' ? 'base' : 'base-sepolia',
    alterMode: partial?.alterMode === 'full' ? 'full' : 'comms',
  }
}
