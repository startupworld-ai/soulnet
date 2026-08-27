/**
 * Right pane for a GROUP item — the room HOST (wire spec §14.7: transport /
 * governance / room). It owns the header (name, member count, role, Home
 * toggle with the application badge, Mark read) and the GROUP HOME (profile
 * summary, rules, pins, members with roles + promote/demote, invite,
 * applications, the public join link, leave/kick), and renders the ROOM named
 * by `profile.room` through the keyed `group.room` slot (declared by the
 * page's registration; the built-in chat room registers the key `chat`, any
 * dsh plugin can register another room key). An unknown room key falls back
 * to the chat room with a notice.
 */
import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import { Button, IconCheckOutline14, IconCloseFill14, IconCopyOutline16, IconUserOutline16, Tooltip, writeClipboard } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsRenderSlots } from '@deepseek-ai/dsh-client-ui-slots'
import { api, networkStore, type ApiGroup, type ApiGroupInfo, type ApiGroupProfile } from './api.ts'
import { canSpeakAs, DEFAULT_ROOM_KEY, encodeGroupUri, roleOf, roomKeyOf, type GroupRole, type RoomOwnerProps } from './group-room.ts'
import { ContentTabs } from './ContentTabs.tsx'
import { formatAge } from './inbox-state.ts'
import { MemoryPane } from './MemoryPane.tsx'
import { tabsFor, type PaneTab } from './page-state.ts'
import { pageStore } from './page-store.ts'
import { ChatRoom } from './rooms/ChatRoom.tsx'
import type { Translate } from './translate.ts'

/** Roster info survives pane switches, so revisiting a group paints names instantly. */
const infoCache = new Map<string, ApiGroupInfo>()

/** 很久没来群（未读 >= 此阈值）进群时，触发一次群记忆提炼（避免每条消息都总结）。 */
const GROUP_MEMORY_MIN_UNREAD = 20

export interface GroupPaneProps {
  t: Translate
  group: ApiGroup
  /** Whether the page is visible (mark-read only then). */
  visible: boolean
  /** Back to "My alter" after leaving. */
  onGoAlter: () => void
  /** The page's `group.room` render authorization, handed down as plain props. */
  renderRoom: PropsRenderSlots<'group.room'>['renderSlot']
}

const templateLabel = (t: Translate, id: string | undefined): string => {
  switch (id) {
    case 'standard': return t('template.standard')
    case 'announcement': return t('template.announcement')
    case 'agents': return t('template.agents')
    case 'tasks': return t('template.tasks')
    case 'casual': return t('template.casual')
    default: return id ?? ''
  }
}
const joinLabel = (t: Translate, join: ApiGroupProfile['join']): string =>
  join === 'apply' ? t('group.form.join.apply') : join === 'open' ? t('group.form.join.open') : t('group.form.join.invite')
const wakeLabel = (t: Translate, wake: ApiGroupProfile['agentWake']): string =>
  wake === 'always' ? t('group.form.wake.always') : wake === 'never' ? t('group.form.wake.never') : t('group.form.wake.mention')
const tierLabel = (t: Translate, tier: ApiGroupProfile['agentTier']): string =>
  tier === 'notify' ? t('tier.short.notify') : tier === 'auto' ? t('tier.short.auto') : t('tier.short.draft')
const whoLabel = (t: Translate, who: ApiGroupProfile['speakWho']): string =>
  who === 'owner' ? t('group.form.speakWho.owner') : who === 'admins' ? t('group.form.speakWho.admins') : t('group.form.speakWho.all')

export function GroupPane({ t, group, visible, onGoAlter, renderRoom }: GroupPaneProps) {
  const gid = group.gid
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const thread = pageStore.groupThread(gid)
  const myFp = net.state?.identity?.fp
  const myName = net.state?.identity?.name ?? 'me'
  const [info, setInfo] = useState<ApiGroupInfo | undefined>(() => infoCache.get(gid))
  const [busy, setBusy] = useState<string | undefined>(undefined)
  const [error, setError] = useState<string | undefined>(undefined)
  const [pinText, setPinText] = useState('')
  const [inviteFp, setInviteFp] = useState('')
  const [copied, setCopied] = useState(false)
  const [editOpen, setEditOpen] = useState(false)
  const [edit, setEdit] = useState({ speakHumans: true, speakAgents: true, speakWho: 'all', join: 'invite', agentWake: 'mention', agentTier: 'draft', rules: '', isPublic: false, tags: '' })
  /** Two-step confirm for destructive actions (leave / kick): first click arms, second fires. */
  const [confirming, setConfirming] = useState<string | undefined>(undefined)
  const confirmThen = (key: string, action: () => void): void => {
    if (confirming === key) {
      setConfirming(undefined)
      action()
      return
    }
    setConfirming(key)
    setTimeout(() => { setConfirming(c => (c === key ? undefined : c)) }, 2500)
  }
  void page // subscribe: thread updates re-render through the snapshot

  const refetchInfo = useCallback(async (): Promise<void> => {
    const { group: g } = await api.groupInfo(gid)
    infoCache.set(gid, g)
    setInfo(g)
  }, [gid])

  // Switching groups (or a roster bump): reset, show the cached roster at once and
  // refresh it in the background (no "loading" flash on revisits).
  useEffect(() => {
    setError(undefined)
    setInfo(infoCache.get(gid))
    setPinText('')
    setInviteFp('')
    setEditOpen(false)
    setConfirming(undefined)
    if (!pageStore.groupThread(gid).loaded) void pageStore.loadGroup(gid)
    let cancelled = false
    api.groupInfo(gid).then(({ group: g }) => {
      infoCache.set(gid, g)
      if (!cancelled) setInfo(g)
    }).catch(() => {})
    return () => { cancelled = true }
  }, [gid, group.version])

  // Entering any management tab (home / bulletin / members / manage — everything
  // but the room): clear the application badge and refresh the authoritative
  // list. Used to hang off `homeOpen`, which the tab strip replaced.
  const onManagementTab = page.paneTab !== 'chat'
  useEffect(() => {
    if (!onManagementTab) return
    networkStore.clearGroupApps(gid)
    void refetchInfo().catch(() => {})
  }, [onManagementTab, gid, refetchInfo])

  // Open + visible = read.
  const unread = group.unread
  useEffect(() => {
    if (!visible || unread === 0) return
    if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return
    void networkStore.markReadGroup(gid)
  }, [gid, unread, visible, thread.entries.length])

  // 埋点：很久没来群（未读多）进群时，触发一次最近消息的记忆提炼。
  useEffect(() => {
    if (group.unread < GROUP_MEMORY_MIN_UNREAD) return
    void api.memorySummarize(gid).catch(() => {})
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gid])

  const run = async (key: string, action: () => Promise<void>): Promise<void> => {
    setBusy(key)
    setError(undefined)
    try {
      await action()
      await refetchInfo().catch(() => {})
      await networkStore.refresh()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(undefined)
    }
  }

  const profile = info?.profile ?? group.profile
  const role: GroupRole = info?.myRole ?? roleOf(group, myFp)
  const canAdmin = role !== 'member'
  const members = info?.memberList ?? []
  const applications = info?.applications ?? []
  const appsBadge = info === undefined ? net.inbox.groupApps[gid] ?? 0 : applications.length
  const roomKey = roomKeyOf(profile)
  const relay = net.status?.relay ?? net.state?.status.relay
  const paneTab: PaneTab = page.paneTab
  const tabs = tabsFor('group', canAdmin)

  const roomOwner: RoomOwnerProps = {
    gid,
    group: { ...group, ...(profile === undefined ? {} : { profile }) },
    me: { fp: myFp ?? '', name: myName },
    members,
    thread,
    actions: {
      send: (body, opts) => pageStore.sendGroup(gid, body, opts),
      loadOlder: () => { void pageStore.loadOlderGroup(gid) },
      reload: () => { void pageStore.loadGroup(gid) },
      markRead: () => { void networkStore.markReadGroup(gid) },
    },
    canSpeakHuman: canSpeakAs(profile, role, 'owner'),
    canSpeakAgent: canSpeakAs(profile, role, 'alter'),
  }

  const setAdmins = (admins: string[]): Promise<void> =>
    api.groupSetProfile(gid, { ...(profile ?? { speakHumans: true, speakAgents: true }), admins }).then(() => {})

  const openEditor = (): void => {
    setEdit({
      speakHumans: profile?.speakHumans ?? true,
      speakAgents: profile?.speakAgents ?? true,
      speakWho: profile?.speakWho ?? 'all',
      join: profile?.join ?? 'invite',
      agentWake: profile?.agentWake ?? 'mention',
      agentTier: profile?.agentTier ?? 'draft',
      rules: profile?.rules ?? '',
      isPublic: profile?.public === true,
      tags: (profile?.tags ?? []).join(', '),
    })
    setEditOpen(true)
  }

  const saveProfile = (): void => {
    const tags = edit.tags.split(',').map(s => s.trim()).filter(s => s !== '')
    void run('profile.save', async () => {
      await api.groupSetProfile(gid, {
        ...(profile ?? {}),
        speakHumans: edit.speakHumans,
        speakAgents: edit.speakAgents,
        speakWho: edit.speakWho as NonNullable<ApiGroupProfile['speakWho']>,
        join: edit.join as NonNullable<ApiGroupProfile['join']>,
        agentWake: edit.agentWake as NonNullable<ApiGroupProfile['agentWake']>,
        agentTier: edit.agentTier as NonNullable<ApiGroupProfile['agentTier']>,
        rules: edit.rules,
        public: edit.isPublic,
        tags,
      })
      setEditOpen(false)
    })
  }

  const inviteCandidates = net.inbox.friends.filter(f => !members.some(m => m.fp === f.fp))

  const copyUri = (): void => {
    if (relay === undefined) return
    void writeClipboard(encodeGroupUri(gid, relay, group.name)).then((ok) => {
      if (!ok) return
      setCopied(true)
      setTimeout(() => { setCopied(false) }, 1500)
    })
  }

  const chips: string[] = profile === undefined ? [] : [
    templateLabel(t, profile.template),
    joinLabel(t, profile.join),
    whoLabel(t, profile.speakWho),
    `${t('group.form.wake')} · ${wakeLabel(t, profile.agentWake)}`,
    `${t('group.form.tier')} · ${tierLabel(t, profile.agentTier)}`,
    ...(profile.speakAgents ? [`${profile.autoPerHour ?? 10}/h`] : []),
    ...(profile.tags ?? []),
  ].filter(s => s !== '')

  const pins = info?.pins ?? []

  const home = (
    <div className="sm-home" data-soulmirror-group-home>
      <div className="sm-home-inner">
        <div className="sm-home-id">
          <span className="sm-avatar" aria-hidden>{group.name.slice(0, 1)}</span>
          <div style={{ display: 'grid', gap: 2, minWidth: 0 }}>
            <span className="sm-home-id-name">{group.name}</span>
            <span className="sm-home-id-sub">
              <span>{t('group.members', { n: group.members })}</span>
              {role === 'owner' ? <span className="sm-rolepill sm-role-owner">{t('group.owner')}</span> : null}
              {role === 'admin' ? <span className="sm-rolepill sm-role-admin">{t('group.home.admin')}</span> : null}
              {roomKey !== DEFAULT_ROOM_KEY ? <span className="sm-statepill">{roomKey}</span> : null}
              {profile?.public === true ? <span className="sm-statepill">{t('group.form.public.short')}</span> : null}
            </span>
          </div>
        </div>
        {info === undefined
          ? <span className="sm-muted" style={{ fontSize: 12, padding: '0 2px' }}>{t('group.home.loading')}</span>
          : null}
        <div className="sm-home-card">
          <div className="sm-home-title">
            <span>{t('group.home.profile')}</span>
            {role === 'owner' && !editOpen
              ? <button type="button" className="sm-ghostbtn" onClick={openEditor} data-soulmirror-group-profile-edit>{t('group.home.edit')}</button>
              : null}
            {relay !== undefined
              ? (
                <button type="button" className="sm-ghostbtn" onClick={copyUri} data-soulmirror-group-uri-copy>
                  {copied ? <IconCheckOutline14 size={14} /> : <IconCopyOutline16 size={14} />} {copied ? t('group.home.uri.copied') : t('group.home.uri.copy')}
                </button>
              )
              : null}
          </div>
          {editOpen
            ? (
              <div style={{ display: 'grid', gap: 10 }} data-soulmirror-group-profile-editor>
                <div className="sm-advgrid">
                  <label className="sm-checkbox">
                    <input type="checkbox" checked={edit.speakHumans} onChange={(e) => { setEdit({ ...edit, speakHumans: e.target.checked }) }} />
                    {t('group.form.speakHumans')}
                  </label>
                  <label className="sm-checkbox">
                    <input type="checkbox" checked={edit.speakAgents} onChange={(e) => { setEdit({ ...edit, speakAgents: e.target.checked }) }} />
                    {t('group.form.speakAgents')}
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.speakWho')}</span>
                    <select className="sm-select" value={edit.speakWho} onChange={(e) => { setEdit({ ...edit, speakWho: e.target.value }) }}>
                      <option value="all">{t('group.form.speakWho.all')}</option>
                      <option value="owner">{t('group.form.speakWho.owner')}</option>
                      <option value="admins">{t('group.form.speakWho.admins')}</option>
                    </select>
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.join')}</span>
                    <select className="sm-select" value={edit.join} onChange={(e) => { setEdit({ ...edit, join: e.target.value }) }}>
                      <option value="invite">{t('group.form.join.invite')}</option>
                      <option value="apply">{t('group.form.join.apply')}</option>
                      <option value="open">{t('group.form.join.open')}</option>
                    </select>
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.wake')}</span>
                    <select className="sm-select" value={edit.agentWake} onChange={(e) => { setEdit({ ...edit, agentWake: e.target.value }) }}>
                      <option value="mention">{t('group.form.wake.mention')}</option>
                      <option value="always">{t('group.form.wake.always')}</option>
                      <option value="never">{t('group.form.wake.never')}</option>
                    </select>
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.tier')}</span>
                    <select className="sm-select" value={edit.agentTier} onChange={(e) => { setEdit({ ...edit, agentTier: e.target.value }) }}>
                      <option value="notify">{t('tier.short.notify')}</option>
                      <option value="draft">{t('tier.short.draft')}</option>
                      <option value="auto">{t('tier.short.auto')}</option>
                    </select>
                  </label>
                </div>
                <label className="sm-field">
                  <span>{t('group.form.rules')}</span>
                  <textarea className="sm-textarea-box" style={{ minHeight: 64 }} value={edit.rules} onChange={(e) => { setEdit({ ...edit, rules: e.target.value }) }} />
                </label>
                <label className="sm-checkbox">
                  <input type="checkbox" checked={edit.isPublic} onChange={(e) => { setEdit({ ...edit, isPublic: e.target.checked }) }} />
                  {t('group.form.public')}
                </label>
                {edit.isPublic
                  ? (
                    <label className="sm-field">
                      <span>{t('group.form.tags')}</span>
                      <input className="sm-input" value={edit.tags} onChange={(e) => { setEdit({ ...edit, tags: e.target.value }) }} />
                    </label>
                  )
                  : null}
                <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
                  <Button variant="outline" size="sm" onClick={() => { setEditOpen(false) }}>{t('group.home.edit.cancel')}</Button>
                  <Button variant="primary" size="sm" disabled={busy !== undefined || (!edit.speakHumans && !edit.speakAgents)} onClick={saveProfile} data-soulmirror-group-profile-save>
                    {t('group.home.edit.save')}
                  </Button>
                </div>
              </div>
            )
            : (
              <>
                {profile === undefined
                  ? <span className="sm-muted" style={{ fontSize: 12 }}>{t('group.home.legacy')}</span>
                  : (
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                      {chips.map((c, i) => <span key={`${c}-${i}`} className="sm-statepill">{c}</span>)}
                    </div>
                  )}
                {profile?.rules !== undefined && profile.rules !== ''
                  ? <div className="sm-home-rules" data-soulmirror-group-rules>{profile.rules}</div>
                  : null}
              </>
            )}
        </div>
        {pins.length > 0 || canAdmin
          ? (
            <div className="sm-home-card">
              <div className="sm-home-title"><span>{t('group.home.pins')}{pins.length > 0 ? ` · ${pins.length}` : ''}</span></div>
              {pins.map(p => (
                <div key={p.id} className="sm-home-pin" data-soulmirror-group-pin={p.id}>
                  <span style={{ flex: 1, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{p.body}</span>
                  <span className="sm-muted" style={{ fontSize: 11 }}>{formatAge(p.ts)}</span>
                  {canAdmin
                    ? (
                      <Tooltip label={t('group.home.pin.remove')} side="top">
                        <button type="button" className="sm-iconbtn" aria-label={t('group.home.pin.remove')} disabled={busy !== undefined} onClick={() => { void run(`unpin:${p.id}`, async () => { await api.groupUnpin(gid, p.id) }) }}>
                          <IconCloseFill14 size={14} />
                        </button>
                      </Tooltip>
                    )
                    : null}
                </div>
              ))}
              {canAdmin
                ? (
                  <div style={{ display: 'flex', gap: 6 }}>
                    <input className="sm-input" placeholder={t('group.home.pin.placeholder')} value={pinText} onChange={(e) => { setPinText(e.target.value) }} data-soulmirror-group-pin-input />
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={busy !== undefined || pinText.trim() === ''}
                      onClick={() => { void run('pin', async () => { await api.groupPin(gid, pinText.trim()); setPinText('') }) }}
                      data-soulmirror-group-pin-add
                    >
                      {t('group.home.pin.add')}
                    </Button>
                  </div>
                )
                : null}
            </div>
          )
          : null}
        {role === 'owner' && applications.length > 0
          ? (
            <div className="sm-home-card">
              <div className="sm-home-title"><span>{t('group.home.applications')} · {applications.length}</span></div>
              {applications.map(a => (
                <div key={a.fp} className="sm-req" style={{ margin: 0 }} data-soulmirror-group-application={a.fp}>
                  <span className="sm-avatar sm-avatar-sm" aria-hidden>{a.name.slice(0, 1)}</span>
                  <span className="sm-row-body">
                    <span className="sm-row-title"><span className="sm-row-name">{a.name}</span>{a.ts !== undefined ? <span className="sm-row-time">{formatAge(a.ts)}</span> : null}</span>
                    <span className="sm-row-preview">{a.note}</span>
                  </span>
                  <span className="sm-pending-actions">
                    <Tooltip label={t('group.home.approve')} side="top">
                      <button type="button" className="sm-iconbtn" aria-label={t('group.home.approve')} disabled={busy !== undefined} onClick={() => { void run(`approve:${a.fp}`, async () => { await api.groupApprove(gid, a.fp) }) }} data-soulmirror-group-approve={a.fp}>
                        <IconCheckOutline14 size={14} />
                      </button>
                    </Tooltip>
                    <Tooltip label={t('group.home.reject')} side="top">
                      <button type="button" className="sm-iconbtn" aria-label={t('group.home.reject')} disabled={busy !== undefined} onClick={() => { void run(`reject:${a.fp}`, async () => { await api.groupApplicationReject(gid, a.fp) }) }}>
                        <IconCloseFill14 size={14} />
                      </button>
                    </Tooltip>
                  </span>
                </div>
              ))}
            </div>
          )
          : null}
        <div className="sm-home-card">
          <div className="sm-home-title"><span>{t('group.home.members')} · {members.length}</span></div>
          <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'grid', gap: 2 }}>
            {members.map((m) => {
              const isOwner = m.fp === group.ownerFp
              const isAdmin = (profile?.admins ?? []).includes(m.fp)
              return (
                <li key={m.fp} className="sm-member">
                  <span className="sm-avatar sm-avatar-sm" aria-hidden>{m.name.slice(0, 1)}</span>
                  <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{m.name}</span>
                  {isOwner ? <span className="sm-rolepill sm-role-owner">{t('group.owner')}</span> : null}
                  {isAdmin ? <span className="sm-rolepill sm-role-admin">{t('group.home.admin')}</span> : null}
                  {m.fp === myFp ? <span className="sm-rolepill sm-role-me">{t('group.sender.me')}</span> : null}
                  {role === 'owner' && !isOwner
                    ? (
                      <>
                        <button
                          type="button"
                          className="sm-linkbtn"
                          disabled={busy !== undefined}
                          onClick={() => {
                            const next = isAdmin ? (profile?.admins ?? []).filter(a => a !== m.fp) : [...(profile?.admins ?? []), m.fp]
                            void run(`admin:${m.fp}`, () => setAdmins(next))
                          }}
                          data-soulmirror-group-admin-toggle={m.fp}
                        >
                          {isAdmin ? t('group.home.demote') : t('group.home.promote')}
                        </button>
                        <Tooltip label={confirming === `kick:${m.fp}` ? t('group.confirm') : t('group.kick')} side="top">
                          <button
                            type="button"
                            className={`sm-iconbtn${confirming === `kick:${m.fp}` ? ' sm-confirming' : ''}`}
                            aria-label={t('group.kick')}
                            disabled={busy !== undefined}
                            onClick={() => { confirmThen(`kick:${m.fp}`, () => { void run(`kick:${m.fp}`, async () => { await api.groupKick(gid, m.fp) }) }) }}
                            data-soulmirror-group-kick={m.fp}
                          >
                            <IconCloseFill14 size={14} />
                          </button>
                        </Tooltip>
                      </>
                    )
                    : null}
                </li>
              )
            })}
          </ul>
          {canAdmin
            ? inviteCandidates.length > 0
              ? (
                <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                  <select className="sm-select" style={{ flex: 1, minWidth: 0 }} value={inviteFp} onChange={(e) => { setInviteFp(e.target.value) }} data-soulmirror-group-invite-pick>
                    <option value="">{t('group.home.invite')}</option>
                    {inviteCandidates.map(f => <option key={f.fp} value={f.fp}>{f.name}</option>)}
                  </select>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy !== undefined || inviteFp === ''}
                    onClick={() => { void run('invite', async () => { await api.groupInvite(gid, inviteFp); setInviteFp('') }) }}
                    data-soulmirror-group-invite
                  >
                    {t('group.home.invite.go')}
                  </Button>
                </div>
              )
              : <span className="sm-muted" style={{ fontSize: 11 }}>{t('group.home.invite.none')}</span>
            : null}
        </div>
        <div style={{ display: 'flex', gap: 6, justifyContent: 'space-between', alignItems: 'center', padding: '0 2px 8px' }}>
          {role === 'owner'
            ? <span className="sm-muted" style={{ fontSize: 11 }}>{t('group.leave.owner')}</span>
            : (
              <button
                type="button"
                className={`sm-ghostbtn sm-dangerbtn${confirming === 'leave' ? ' sm-confirming' : ''}`}
                disabled={busy !== undefined}
                onClick={() => { confirmThen('leave', () => { void run('leave', async () => { await api.groupLeave(gid); onGoAlter() }) }) }}
                data-soulmirror-group-leave
              >
                {confirming === 'leave' ? t('group.confirm') : t('group.leave')}
              </button>
            )}
          <button type="button" className="sm-ghostbtn" onClick={() => { pageStore.setPaneTab('chat') }}>{t('inbox.close')}</button>
        </div>
        {error !== undefined ? <span style={{ fontSize: 12, color: 'var(--dsw-alias-state-error-primary)' }}>{t('settings.error', { message: error })}</span> : null}
      </div>
    </div>
  )

  const homeId = (
    <div className="sm-home-id">
      <span className="sm-avatar" aria-hidden>{group.name.slice(0, 1)}</span>
      <div style={{ display: 'grid', gap: 2, minWidth: 0 }}>
        <span className="sm-home-id-name">{group.name}</span>
        <span className="sm-home-id-sub">
          <span>{t('group.members', { n: group.members })}</span>
          {role === 'owner' ? <span className="sm-rolepill sm-role-owner">{t('group.owner')}</span> : null}
          {role === 'admin' ? <span className="sm-rolepill sm-role-admin">{t('group.home.admin')}</span> : null}
          {roomKey !== DEFAULT_ROOM_KEY ? <span className="sm-statepill">{roomKey}</span> : null}
          {profile?.public === true ? <span className="sm-statepill">{t('group.form.public.short')}</span> : null}
        </span>
      </div>
    </div>
  )
  const panelError = error !== undefined ? <span style={{ fontSize: 12, color: 'var(--dsw-alias-state-error-primary)' }}>{t('settings.error', { message: error })}</span> : null
  const leaveKickFooter = (
    <div style={{ display: 'flex', gap: 6, justifyContent: 'space-between', alignItems: 'center', padding: '0 2px 8px' }}>
      {role === 'owner'
        ? <span className="sm-muted" style={{ fontSize: 11 }}>{t('group.leave.owner')}</span>
        : (
          <button
            type="button"
            className={`sm-ghostbtn sm-dangerbtn${confirming === 'leave' ? ' sm-confirming' : ''}`}
            disabled={busy !== undefined}
            onClick={() => { confirmThen('leave', () => { void run('leave', async () => { await api.groupLeave(gid); onGoAlter() }) }) }}
            data-soulmirror-group-leave
          >
            {confirming === 'leave' ? t('group.confirm') : t('group.leave')}
          </button>
        )}
      <button type="button" className="sm-ghostbtn" onClick={() => { pageStore.setPaneTab('chat') }}>{t('inbox.close')}</button>
    </div>
  )
  const applicationsCard = role === 'owner' && applications.length > 0
    ? (
      <div className="sm-home-card">
        <div className="sm-home-title"><span>{t('group.home.applications')} · {applications.length}</span></div>
        {applications.map(a => (
          <div key={a.fp} className="sm-req" style={{ margin: 0 }} data-soulmirror-group-application={a.fp}>
            <span className="sm-avatar sm-avatar-sm" aria-hidden>{a.name.slice(0, 1)}</span>
            <span className="sm-row-body">
              <span className="sm-row-title"><span className="sm-row-name">{a.name}</span>{a.ts !== undefined ? <span className="sm-row-time">{formatAge(a.ts)}</span> : null}</span>
              <span className="sm-row-preview">{a.note}</span>
            </span>
            <span className="sm-pending-actions">
              <Tooltip label={t('group.home.approve')} side="top">
                <button type="button" className="sm-iconbtn" aria-label={t('group.home.approve')} disabled={busy !== undefined} onClick={() => { void run(`approve:${a.fp}`, async () => { await api.groupApprove(gid, a.fp) }) }} data-soulmirror-group-approve={a.fp}>
                  <IconCheckOutline14 size={14} />
                </button>
              </Tooltip>
              <Tooltip label={t('group.home.reject')} side="top">
                <button type="button" className="sm-iconbtn" aria-label={t('group.home.reject')} disabled={busy !== undefined} onClick={() => { void run(`reject:${a.fp}`, async () => { await api.groupApplicationReject(gid, a.fp) }) }}>
                  <IconCloseFill14 size={14} />
                </button>
              </Tooltip>
            </span>
          </div>
        ))}
      </div>
    )
    : null

  /** 公告 tab: pinned notes + group rules. */
  const announce = (
    <div className="sm-home" data-soulmirror-group-announce>
      <div className="sm-home-inner">
        {homeId}
        {profile?.rules !== undefined && profile.rules !== ''
          ? (
            <div className="sm-home-card">
              <div className="sm-home-title"><span>{t('group.home.rules')}</span></div>
              <div className="sm-home-rules" data-soulmirror-group-rules>{profile.rules}</div>
            </div>
          )
          : null}
        {pins.length > 0 || canAdmin
          ? (
            <div className="sm-home-card">
              <div className="sm-home-title"><span>{t('group.home.pins')}{pins.length > 0 ? ` · ${pins.length}` : ''}</span></div>
              {pins.map(p => (
                <div key={p.id} className="sm-home-pin" data-soulmirror-group-pin={p.id}>
                  <span style={{ flex: 1, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{p.body}</span>
                  <span className="sm-muted" style={{ fontSize: 11 }}>{formatAge(p.ts)}</span>
                  {canAdmin
                    ? (
                      <Tooltip label={t('group.home.pin.remove')} side="top">
                        <button type="button" className="sm-iconbtn" aria-label={t('group.home.pin.remove')} disabled={busy !== undefined} onClick={() => { void run(`unpin:${p.id}`, async () => { await api.groupUnpin(gid, p.id) }) }}>
                          <IconCloseFill14 size={14} />
                        </button>
                      </Tooltip>
                    )
                    : null}
                </div>
              ))}
              {canAdmin
                ? (
                  <div style={{ display: 'flex', gap: 6 }}>
                    <input className="sm-input" placeholder={t('group.home.pin.placeholder')} value={pinText} onChange={(e) => { setPinText(e.target.value) }} data-soulmirror-group-pin-input />
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={busy !== undefined || pinText.trim() === ''}
                      onClick={() => { void run('pin', async () => { await api.groupPin(gid, pinText.trim()); setPinText('') }) }}
                      data-soulmirror-group-pin-add
                    >
                      {t('group.home.pin.add')}
                    </Button>
                  </div>
                )
                : null}
            </div>
          )
          : null}
        {leaveKickFooter}
        {panelError}
      </div>
    </div>
  )

  /** 成员 tab: roster + invite + role management. */
  const membersPane = (
    <div className="sm-home" data-soulmirror-group-members>
      <div className="sm-home-inner">
        {homeId}
        <div className="sm-home-card">
          <div className="sm-home-title"><span>{t('group.home.members')} · {members.length}</span></div>
          <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'grid', gap: 2 }}>
            {members.map((m) => {
              const isOwner = m.fp === group.ownerFp
              const isAdmin = (profile?.admins ?? []).includes(m.fp)
              return (
                <li key={m.fp} className="sm-member">
                  <span className="sm-avatar sm-avatar-sm" aria-hidden>{m.name.slice(0, 1)}</span>
                  <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{m.name}</span>
                  {isOwner ? <span className="sm-rolepill sm-role-owner">{t('group.owner')}</span> : null}
                  {isAdmin ? <span className="sm-rolepill sm-role-admin">{t('group.home.admin')}</span> : null}
                  {m.fp === myFp ? <span className="sm-rolepill sm-role-me">{t('group.sender.me')}</span> : null}
                  {role === 'owner' && !isOwner
                    ? (
                      <>
                        <button
                          type="button"
                          className="sm-linkbtn"
                          disabled={busy !== undefined}
                          onClick={() => {
                            const next = isAdmin ? (profile?.admins ?? []).filter(a => a !== m.fp) : [...(profile?.admins ?? []), m.fp]
                            void run(`admin:${m.fp}`, () => setAdmins(next))
                          }}
                          data-soulmirror-group-admin-toggle={m.fp}
                        >
                          {isAdmin ? t('group.home.demote') : t('group.home.promote')}
                        </button>
                        <Tooltip label={confirming === `kick:${m.fp}` ? t('group.confirm') : t('group.kick')} side="top">
                          <button
                            type="button"
                            className={`sm-iconbtn${confirming === `kick:${m.fp}` ? ' sm-confirming' : ''}`}
                            aria-label={t('group.kick')}
                            disabled={busy !== undefined}
                            onClick={() => { confirmThen(`kick:${m.fp}`, () => { void run(`kick:${m.fp}`, async () => { await api.groupKick(gid, m.fp) }) }) }}
                            data-soulmirror-group-kick={m.fp}
                          >
                            <IconCloseFill14 size={14} />
                          </button>
                        </Tooltip>
                      </>
                    )
                    : null}
                </li>
              )
            })}
          </ul>
          {canAdmin
            ? inviteCandidates.length > 0
              ? (
                <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                  <select className="sm-select" style={{ flex: 1, minWidth: 0 }} value={inviteFp} onChange={(e) => { setInviteFp(e.target.value) }} data-soulmirror-group-invite-pick>
                    <option value="">{t('group.home.invite')}</option>
                    {inviteCandidates.map(f => <option key={f.fp} value={f.fp}>{f.name}</option>)}
                  </select>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy !== undefined || inviteFp === ''}
                    onClick={() => { void run('invite', async () => { await api.groupInvite(gid, inviteFp); setInviteFp('') }) }}
                    data-soulmirror-group-invite
                  >
                    {t('group.home.invite.go')}
                  </Button>
                </div>
              )
              : <span className="sm-muted" style={{ fontSize: 11 }}>{t('group.home.invite.none')}</span>
            : null}
        </div>
        {applicationsCard}
        {leaveKickFooter}
        {panelError}
      </div>
    </div>
  )

  /** 管理 tab (owner/admin): group settings + applications + leave. */
  const admin = (
    <div className="sm-home" data-soulmirror-group-admin>
      <div className="sm-home-inner">
        {homeId}
        <div className="sm-home-card">
          <div className="sm-home-title">
            <span>{t('group.home.profile')}</span>
            {relay !== undefined
              ? (
                <button type="button" className="sm-ghostbtn" onClick={copyUri} data-soulmirror-group-uri-copy>
                  {copied ? <IconCheckOutline14 size={14} /> : <IconCopyOutline16 size={14} />} {copied ? t('group.home.uri.copied') : t('group.home.uri.copy')}
                </button>
              )
              : null}
          </div>
          {editOpen
            ? (
              <div style={{ display: 'grid', gap: 10 }} data-soulmirror-group-profile-editor>
                <div className="sm-advgrid">
                  <label className="sm-checkbox">
                    <input type="checkbox" checked={edit.speakHumans} onChange={(e) => { setEdit({ ...edit, speakHumans: e.target.checked }) }} />
                    {t('group.form.speakHumans')}
                  </label>
                  <label className="sm-checkbox">
                    <input type="checkbox" checked={edit.speakAgents} onChange={(e) => { setEdit({ ...edit, speakAgents: e.target.checked }) }} />
                    {t('group.form.speakAgents')}
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.speakWho')}</span>
                    <select className="sm-select" value={edit.speakWho} onChange={(e) => { setEdit({ ...edit, speakWho: e.target.value }) }}>
                      <option value="all">{t('group.form.speakWho.all')}</option>
                      <option value="owner">{t('group.form.speakWho.owner')}</option>
                      <option value="admins">{t('group.form.speakWho.admins')}</option>
                    </select>
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.join')}</span>
                    <select className="sm-select" value={edit.join} onChange={(e) => { setEdit({ ...edit, join: e.target.value }) }}>
                      <option value="invite">{t('group.form.join.invite')}</option>
                      <option value="apply">{t('group.form.join.apply')}</option>
                      <option value="open">{t('group.form.join.open')}</option>
                    </select>
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.wake')}</span>
                    <select className="sm-select" value={edit.agentWake} onChange={(e) => { setEdit({ ...edit, agentWake: e.target.value }) }}>
                      <option value="mention">{t('group.form.wake.mention')}</option>
                      <option value="always">{t('group.form.wake.always')}</option>
                      <option value="never">{t('group.form.wake.never')}</option>
                    </select>
                  </label>
                  <label className="sm-field">
                    <span>{t('group.form.tier')}</span>
                    <select className="sm-select" value={edit.agentTier} onChange={(e) => { setEdit({ ...edit, agentTier: e.target.value }) }}>
                      <option value="notify">{t('tier.short.notify')}</option>
                      <option value="draft">{t('tier.short.draft')}</option>
                      <option value="auto">{t('tier.short.auto')}</option>
                    </select>
                  </label>
                </div>
                <label className="sm-field">
                  <span>{t('group.form.rules')}</span>
                  <textarea className="sm-textarea-box" style={{ minHeight: 64 }} value={edit.rules} onChange={(e) => { setEdit({ ...edit, rules: e.target.value }) }} />
                </label>
                <label className="sm-checkbox">
                  <input type="checkbox" checked={edit.isPublic} onChange={(e) => { setEdit({ ...edit, isPublic: e.target.checked }) }} />
                  {t('group.form.public')}
                </label>
                {edit.isPublic
                  ? (
                    <label className="sm-field">
                      <span>{t('group.form.tags')}</span>
                      <input className="sm-input" value={edit.tags} onChange={(e) => { setEdit({ ...edit, tags: e.target.value }) }} />
                    </label>
                  )
                  : null}
                <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
                  <Button variant="outline" size="sm" onClick={() => { setEditOpen(false) }}>{t('group.home.edit.cancel')}</Button>
                  <Button variant="primary" size="sm" disabled={busy !== undefined || (!edit.speakHumans && !edit.speakAgents)} onClick={saveProfile} data-soulmirror-group-profile-save>
                    {t('group.home.edit.save')}
                  </Button>
                </div>
              </div>
            )
            : (
              <div style={{ display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'wrap' }}>
                {profile === undefined
                  ? <span className="sm-muted" style={{ fontSize: 12 }}>{t('group.home.legacy')}</span>
                  : (
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                      {chips.map((c, i) => <span key={`${c}-${i}`} className="sm-statepill">{c}</span>)}
                    </div>
                  )}
                {role === 'owner'
                  ? <button type="button" className="sm-ghostbtn" onClick={openEditor} data-soulmirror-group-profile-edit style={{ marginLeft: 'auto' }}>{t('group.home.edit')}</button>
                  : null}
              </div>
            )}
        </div>
        {applicationsCard}
        {leaveKickFooter}
        {panelError}
      </div>
    </div>
  )

  return (
    <section className="sm-chat-col" data-soulmirror-group-chat={gid} style={{ position: 'relative' }}>
      <header className="sm-chat-head">
        <span className="sm-avawrap">
          <span className="sm-avatar sm-avatar-lg" aria-hidden>{group.name.slice(0, 1)}</span>
        </span>
        <div style={{ flex: 1, minWidth: 0, display: 'grid' }}>
          <div className="sm-chat-head-name">{group.name}</div>
          <div className="sm-chat-head-sub">
            {t('group.members', { n: group.members })}
            {role === 'owner' ? <span> · {t('group.owner')}</span> : role === 'admin' ? <span> · {t('group.home.admin')}</span> : null}
            {roomKey !== DEFAULT_ROOM_KEY ? <span> · {roomKey}</span> : null}
          </div>
        </div>
        <div className="sm-chat-head-actions">
          <button type="button" className="sm-ghostbtn" onClick={() => { pageStore.setPaneTab('home') }} aria-expanded={paneTab === 'home'} data-soulmirror-group-home-toggle>
            <IconUserOutline16 size={14} /> {t('group.home')}
            {appsBadge > 0 ? <span className="sm-badge sm-badge-warn" data-soulmirror-group-apps={appsBadge}>{appsBadge}</span> : null}
          </button>
        </div>
      </header>
      <ContentTabs tabs={tabs} active={paneTab} onChange={pageStore.setPaneTab} t={t} />
      {paneTab === 'chat'
        ? renderRoom('group.room', roomOwner, {
          entryKey: roomKey,
          // No occupant for this room key: fall back to the built-in chat with a notice.
          fallback: (
            <>
              {roomKey !== DEFAULT_ROOM_KEY ? <div className="sm-banner" data-soulmirror-group-room-missing={roomKey}>{t('group.room.missing', { id: roomKey })}</div> : null}
              <ChatRoom {...roomOwner} t={t} />
            </>
          ),
        })
        : paneTab === 'announce' ? announce
        : paneTab === 'memory' ? <MemoryPane t={t} allow={{ global: true, group: gid }} scope={{ kind: 'shared-group', gid }} />
        : paneTab === 'members' ? membersPane
        : paneTab === 'admin' ? admin
        : home}
    </section>
  )
}
