/**
 * soulnet-dsh browser half (served as
 * /plugins/soulnet-dsh/client.js).
 *
 * Registers: zh/en dictionaries, the `a2a-message` Conversation Node
 * Definition + keyed Chat renderer (inbound mail bubbles / plugin notes in
 * the native alter session), the `sidebar.footer.action` entry (SoulMirror
 * button + unread badge → toggles the page; moves into `sidebar.nav.primary`
 * when the SoulMirror sidebar is installed), two `shell.overlay` entries (`soulmirror-page`:
 * the full page — the pinned "My alter" chat, the friend list and the
 * read-only friend threads, right of dsh's sidebar; `soulmirror-inbox`: the
 * new-mail toast), the `settings.section` "SoulMirror network", the
 * `settings.onboarding` identity step, and the client side of the slash
 * commands (`/friends` and `/soulmirror` popups open the page). The P3
 * composer takeover of friend sessions is gone with the friend sessions
 * (P4): the alter session keeps dsh's native composer. All cross-plugin
 * collaboration goes through cordis services (`ctx.slots`, `ctx.locale`,
 * `ctx.conversationEvents`, `ctx.sessions`, `ctx.settingsScope`,
 * `ctx.commandUi`); the only value imports are react, ui-primitives and this
 * package's own files (bundle purity).
 */
import type { ClientContext, SessionId } from '@deepseek-ai/dsh-client-runtime/client'
// Type-only: ctx.locale, ctx.commandUi, ctx.settingsScope, the conversation/settings SlotMap rows.
import type {} from '@deepseek-ai/dsh-client-locale/client'
import type {} from '@deepseek-ai/dsh-client-ui-commands/client'
import type {} from '@deepseek-ai/dsh-client-ui-conversation/client'
import type {} from '@deepseek-ai/dsh-client-ui-layout/client'
import type {} from '@deepseek-ai/dsh-client-ui-settings/client'
import type {} from '@deepseek-ai/dsh-client-ui-sidebar/client'
import type {} from '@deepseek-ai/dsh-client-ui-theme/client'
// Type-only: the `sidebar.nav.primary` seat of the SoulMirror sidebar (optional peer; absent on a stock dsh).
import type {} from 'soulnet-dsh-sidebar/client'
import { a2aRelayDefinition } from './a2a-node.ts'
import { A2ANode } from './A2ANode.tsx'
import { installBranding } from './Branding.tsx'
import { api } from './api.ts'
// Value import of the SlotMap merge modules: keeps the `group.room` and
// `alter.card` declarations in the bundle.
import type {} from './group-room.ts'
import type {} from './alter-card.ts'
import { AlterSettingsCard } from './AlterSettingsCard.tsx'
import { setDirectoryPicker } from './dir-picker.ts'
import { InboxOverlay, type InboxOverlayInjected } from './InboxOverlay.tsx'
import { en, NS, zh } from './locales.ts'
import { ChatRoom } from './rooms/ChatRoom.tsx'
import { navSeatStore } from './nav-seat.ts'
import { SoulmirrorOnboarding } from './Onboarding.tsx'
import { SoulmirrorSettingsSection, type SoulmirrorSettingsInjected, type SoulmirrorSettingsValues } from './SettingsSection.tsx'
import { pageStore } from './page-store.ts'
import { SidebarEntry } from './SidebarEntry.tsx'
import { SidebarNavEntry } from './SidebarNavEntry.tsx'
import { SoulmirrorPage, type SoulmirrorPageInjected } from './SoulmirrorPage.tsx'
import { UpdateAction } from './UpdateAction.tsx'
import { upgradeStore } from './upgrade-store.ts'
import { ensureStyles, removeStyles } from './styles.ts'

export type { A2AChatData } from './a2a-node.ts'
export type { SoulmirrorKey } from './locales.ts'
export type { InboxState, MailNotice } from './inbox-state.ts'
export type { ThreadEntry, ThreadState } from './page-state.ts'
// The `group.room` seat contract, for third-party room plugins ("How to write
// a room plugin" in the README). Importing this entry also merges the SlotMap row.
export type { RoomActions, RoomMember, RoomOwnerProps, RoomThread } from './group-room.ts'
export { DEFAULT_ROOM_KEY, roomKeyOf } from './group-room.ts'

const SETTINGS_NAMESPACE = 'soulmirror'

export const inject = ['slots', 'locale', 'conversationEvents', 'sessions', 'settingsScope', 'commandUi', 'theme', 'workspaces']

/** Whether a session cwd is the SoulMirror workspace (`<home>/a2a`, e.g. `~/.soulnet/a2a`). */
export function isSoulmirrorCwd(cwd: string | undefined): boolean {
  if (cwd === undefined) return false
  const normalized = cwd.replace(/\\/g, '/').replace(/\/+$/, '')
  return /\/\.soulnet\/a2a$/.test(normalized) || /\/soulmirror\/a2a$/.test(normalized) || /\/\.soulmirror\/a2a$/.test(normalized)
}

export function apply(ctx: ClientContext): void {
  ctx.effect(() => ctx.locale.register(NS, { zh, en }), 'soulmirror: dictionaries')
  const t = ctx.locale.bind(NS)
  setDirectoryPicker(() => ctx.workspaces.pickDirectory())
  ctx.effect(() => {
    ensureStyles()
    return removeStyles
  }, 'soulmirror: inbox styles')
  // SoulMirror branding (sidebar brand slots, title, favicon) + the
  // Discord-flavoured dark palette over the whole shell (./Branding.tsx).
  installBranding(ctx)
  const openSession = (id: string): void => { ctx.sessions.open(id as SessionId) }

  // 1. Inbound mail / plugin notes → `a2a-message` nodes (engine), then the
  //    keyed renderer (slot) for the native alter session view.
  ctx.conversationEvents.register(a2aRelayDefinition)
  ctx.slots.inject('conversation.chat.node', () => ctx.slots.register({
    name: 'conversation.chat.node',
    key: 'a2a-message',
    locale: NS,
  }, A2ANode))

  // (The P2 "SoulMirror" view tab beside Chat/Trajectory is gone in P5: the
  //  page + the sidebar entry are the way in.)

  // 2b. Sidebar foot: the "SoulMirror" button with the unread badge (stacked
  //     above Settings in both widths) toggles the SoulMirror page …
  ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({
    name: 'sidebar.footer.action',
    id: 'soulmirror',
    order: 0,
    locale: NS,
  }, SidebarEntry))
  //     A one-click upgrade button appears in the same foot stack whenever a
  //     newer release is known (owner-requested: a real button, not a dot).
  ctx.slots.inject('sidebar.footer.action', () => ctx.slots.register({
    name: 'sidebar.footer.action',
    id: 'soulmirror-update',
    order: 1,
    locale: NS,
  }, UpdateAction))
  //     … and, when the SoulMirror sidebar (soulnet-dsh-sidebar) is installed,
  //     the same entry as the first primary-nav row under New Session; the
  //     foot entry then hides itself (nav-seat.ts). This inject callback only
  //     fires once that sidebar has declared the seat — on a stock dsh it
  //     never runs and the foot entry stays.
  ctx.slots.inject('sidebar.nav.primary', () => {
    navSeatStore.set(true)
    const dispose = ctx.slots.register({
      name: 'sidebar.nav.primary',
      id: 'soulmirror',
      order: 0,
      locale: NS,
    }, SidebarNavEntry)
    return () => {
      navSeatStore.set(false)
      dispose()
    }
  })

  // 2c. … which lives in the root overlay layer: the full page right of dsh's
  //     sidebar (the settings scope is bound once below; the page reads
  //     `directSend` from it live)
  const scope = ctx.settingsScope.bind<SoulmirrorSettingsValues>({ namespace: SETTINGS_NAMESPACE })
  const pageInjected = (): SoulmirrorPageInjected => ({ openSession, scope })
  ctx.slots.inject('shell.overlay', () => ctx.slots.register({
    name: 'shell.overlay',
    id: 'soulmirror-page',
    order: 40,
    locale: NS,
    // The page declares the `group.room` seat (rooms: the pluggable apps
    // rendering a group) AND the `alter.card` seat (cards: the pluggable
    // modules on the alter's home tab).
    children: { 'group.room': { kind: 'keyed', scope: 'root' }, 'alter.card': { kind: 'list', scope: 'root' } },
    inject: pageInjected,
  }, SoulmirrorPage))

  // 2c-bis. The built-in CHAT room occupies `group.room` under the key `chat`
  //         through the standard slot API — a third-party dsh plugin ships
  //         another room by registering another key the same way.
  ctx.slots.inject('group.room', () => ctx.slots.register({
    name: 'group.room',
    key: 'chat',
    locale: NS,
  }, ChatRoom))

  // 2c-ter. The built-in alter cards: the alter's own settings (and, later,
  //         memory / skills / group-memory …) are pluggable cards on the alter's
  //         home tab — a third-party dsh plugin adds another card by registering
  //         into `alter.card` the same way.
  ctx.slots.inject('alter.card', () => ctx.slots.register({
    name: 'alter.card',
    id: 'settings',
    locale: NS,
  }, AlterSettingsCard))

  // 2d. … and the new-mail toast (mail while the alter session is not on
  //     screen and the friend's thread is not open on the page).
  const overlayInjected = (): InboxOverlayInjected => ({
    currentSessionId: () => ctx.sessions.list.getSnapshot().current,
  })
  ctx.slots.inject('shell.overlay', () => ctx.slots.register({
    name: 'shell.overlay',
    id: 'soulmirror-inbox',
    order: 50,
    locale: NS,
    inject: overlayInjected,
  }, InboxOverlay))

  // 3. Settings section backed by the host `soulmirror` namespace. One SILENT
  //    update check per page load; a newer published version puts a dot on
  //    the section's nav label (the store answers before Settings is opened)
  //    and the full badge inside the "Version & updates" card.
  void upgradeStore.check({ silent: true })
  // A page left open would otherwise never learn of a new release: re-check
  // silently every 5 minutes (a lightweight metadata GET; the badge appears
  // by itself).
  ctx.effect(() => {
    const timer = setInterval(() => { void upgradeStore.check({ silent: true, refresh: true }) }, 5 * 60_000)
    return () => { clearInterval(timer) }
  })
  const settingsInjected = (): SoulmirrorSettingsInjected => ({
    openSession,
    scope,
  })
  ctx.slots.inject('settings.section', () => {
    // The host evaluates a slot label ONCE at registration - the silent
    // update check answers later, so a label function alone never grows its
    // dot. Re-register the section whenever hasUpdate flips.
    const make = (): (() => void) => ctx.slots.register({
      name: 'settings.section',
      id: 'soulmirror',
      order: 80,
      locale: NS,
      label: () => upgradeStore.getSnapshot().hasUpdate ? `${t('settings.nav')} ●` : t('settings.nav'),
      inject: settingsInjected,
    }, SoulmirrorSettingsSection)
    let hadUpdate = upgradeStore.getSnapshot().hasUpdate
    let disposeSection = make()
    const unsubscribe = upgradeStore.subscribe(() => {
      const now = upgradeStore.getSnapshot().hasUpdate
      if (now === hadUpdate) return
      hadUpdate = now
      disposeSection()
      disposeSection = make()
    })
    return () => {
      unsubscribe()
      disposeSection()
    }
  })

  // 4. First-run onboarding: create the identity (after dsh's own welcome/model steps).
  ctx.slots.inject('settings.onboarding', () => ctx.slots.register({
    name: 'settings.onboarding',
    id: 'soulmirror-identity',
    order: 50,
    locale: NS,
  }, SoulmirrorOnboarding))

  // 5. Slash commands, client side: `/friends` (host command) gets a popup on
  //    bare invocation that opens the page on the chosen friend; `/soulmirror`
  //    gets a popup that opens the page (on "My alter" or one friend). `/card`
  //    and `/add <card_uri>` are plain host commands (their text result is
  //    rendered by the composer).
  const friendOptions = async (): Promise<{ id: string; label: string; detail: string }[]> => {
    const state = await api.state().catch(() => undefined)
    if (state === undefined || state.friends.length === 0) return []
    return state.friends.map(f => ({
      id: f.fp,
      label: `${f.online === true ? '● ' : '○ '}${f.name}${f.unread > 0 ? ` (${f.unread})` : ''}${(f.drafts ?? 0) > 0 ? ` · ${t('page.row.draftTag')}` : ''}`,
      detail: t('cmd.friends.open', { name: f.name }),
    }))
  }
  ctx.effect(() => ctx.commandUi.decorate({
    name: 'friends',
    available: () => true,
    ui: {
      kind: 'popupSelect',
      options: async () => {
        const options = await friendOptions()
        return options.length === 0 ? [{ id: 'none', label: t('cmd.friends.none') }] : options
      },
      onSelect: (option) => {
        if (option.id === 'none') return
        pageStore.open(option.id)
      },
    },
  }), 'soulmirror: /friends popup')
  ctx.effect(() => ctx.commandUi.decorate({
    name: 'soulmirror',
    available: () => true,
    ui: {
      kind: 'popupSelect',
      options: async () => {
        const page = { id: 'page', label: t('cmd.soulmirror.page'), detail: t('cmd.soulmirror.page.detail') }
        return [page, ...await friendOptions()]
      },
      onSelect: (option) => {
        pageStore.open(option.id === 'page' ? 'alter' : option.id)
      },
    },
  }), 'soulmirror: /soulmirror popup')
}
