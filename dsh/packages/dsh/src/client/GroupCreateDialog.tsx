/**
 * The create-group dialog: a centered modal over the SoulMirror page (the
 * 300px list column is too narrow for a form of this size). Name, member
 * multi-select (friends, with avatars), the five template cards, and a
 * collapsible advanced section (governance switches, rules, public+tags).
 * The dialog owns the form state; the caller owns the actual creation
 * (`onCreate`) and the busy flag.
 */
import { useEffect, useState } from 'react'
import { Button, IconCloseOutline16 } from '@deepseek-ai/dsh-client-ui-primitives'
import type { ApiGroupProfile } from './api.ts'
import { GROUP_TEMPLATES, templateProfile } from './group-templates.ts'

import type { InboxFriend } from './inbox-state.ts'
import type { Translate } from './translate.ts'

export interface GroupCreateDialogProps {
  t: Translate
  friends: readonly InboxFriend[]
  busy: boolean
  /** The owner's wallet address; undefined = no wallet yet (paid join disabled). */
  walletAddress?: string
  onCreate: (name: string, members: string[], profile: ApiGroupProfile) => void
  onClose: () => void
}

export function GroupCreateDialog({ t, friends, busy, walletAddress, onCreate, onClose }: GroupCreateDialogProps) {
  const std = GROUP_TEMPLATES[0]!.profile
  const [name, setName] = useState('')
  const [sel, setSel] = useState<Record<string, boolean>>({})
  // Template picking is parked for now (the framework stays — see group-templates.ts):
  // every group starts from the standard template; the advanced section customizes it.
  const tpl = 'standard'
  const [advOpen, setAdvOpen] = useState(false)
  const [adv, setAdv] = useState({
    speakHumans: std.speakHumans,
    speakAgents: std.speakAgents,
    speakWho: std.speakWho ?? 'all',
    join: std.join ?? 'invite',
    joinPrice: '',
    agentWake: std.agentWake ?? 'mention',
    agentTier: std.agentTier ?? 'draft',
    rules: '',
    isPublic: false,
    tags: '',
  })

  // Escape closes the dialog (the page's own Escape handler yields while it is open).
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('keydown', onKey) }
  }, [onClose])

  const members = Object.entries(sel).filter(([, v]) => v).map(([k]) => k)
  // Paid join needs a wallet AND a price; without them the group cannot be paid.
  const paidIncomplete = adv.join === 'paid' && (walletAddress === undefined || adv.joinPrice.trim() === '')
  const ready = !busy && name.trim() !== '' && members.length > 0 && !paidIncomplete

  const submit = (): void => {
    if (!ready) return
    const tags = adv.tags.split(',').map(s => s.trim()).filter(s => s !== '')
    const paid = adv.join === 'paid' && walletAddress !== undefined && adv.joinPrice.trim() !== ''
    const profile = templateProfile(tpl, {
      speakHumans: adv.speakHumans,
      speakAgents: adv.speakAgents,
      speakWho: adv.speakWho,
      // Native paid join (wire spec §14.7): join=paid + published
      // join_price/join_addr (the relay must accept join=paid).
      join: adv.join,
      agentWake: adv.agentWake,
      agentTier: adv.agentTier,
      ...(adv.rules.trim() === '' ? {} : { rules: adv.rules }),
      ...(paid ? { joinPrice: adv.joinPrice.trim(), joinAddr: walletAddress } : {}),
      ...(adv.isPublic ? { public: true } : {}),
      ...(tags.length === 0 ? {} : { tags }),
    })
    onCreate(name.trim(), members, profile)
  }

  return (
    <div className="sm-modal-backdrop" role="presentation" onClick={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <div className="sm-modal" role="dialog" aria-label={t('group.create')} data-soulmirror-modal data-soulmirror-group-create>
        <div className="sm-modal-head">
          <span className="sm-modal-title">{t('group.create')}</span>
          <button type="button" className="sm-iconbtn" aria-label={t('inbox.close')} onClick={onClose}>
            <IconCloseOutline16 size={14} />
          </button>
        </div>
        <label className="sm-field">
          <span>{t('group.create.name')}</span>
          <input
            className="sm-input sm-input-lg"
            placeholder={t('group.create.name')}
            value={name}
            autoFocus
            onChange={(e) => { setName(e.target.value) }}
            onKeyDown={(e) => { if (e.key === 'Enter' && !e.nativeEvent.isComposing) submit() }}
            data-soulmirror-group-create-name
          />
        </label>
        <div className="sm-field">
          <span>{t('group.create.members')}{members.length > 0 ? ` · ${members.length}` : ''}</span>
          {friends.length === 0
            ? <span className="sm-muted" style={{ fontSize: 12 }}>{t('group.create.needFriends')}</span>
            : (
              <div className="sm-memberpick">
                {friends.map(f => (
                  <label key={f.fp}>
                    <input
                      type="checkbox"
                      checked={sel[f.fp] === true}
                      onChange={(e) => { setSel({ ...sel, [f.fp]: e.target.checked }) }}
                      data-soulmirror-group-create-member={f.fp}
                    />
                    <span className="sm-avatar sm-avatar-sm" aria-hidden>{f.name.slice(0, 1)}</span>
                    <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{f.name}</span>
                  </label>
                ))}
              </div>
            )}
        </div>
        <button type="button" className="sm-linkbtn" style={{ justifySelf: 'start' }} aria-expanded={advOpen} onClick={() => { setAdvOpen(v => !v) }} data-soulmirror-group-create-advanced-toggle>
          {advOpen ? '▾' : '▸'} {t('group.create.advanced')}
        </button>
        {advOpen
          ? (
            <div style={{ display: 'grid', gap: 10 }} data-soulmirror-group-create-advanced>
              <div className="sm-advgrid">
                <label className="sm-checkbox">
                  <input type="checkbox" checked={adv.speakHumans} onChange={(e) => { setAdv({ ...adv, speakHumans: e.target.checked }) }} />
                  {t('group.form.speakHumans')}
                </label>
                <label className="sm-checkbox">
                  <input type="checkbox" checked={adv.speakAgents} onChange={(e) => { setAdv({ ...adv, speakAgents: e.target.checked }) }} />
                  {t('group.form.speakAgents')}
                </label>
                <label className="sm-field">
                  <span>{t('group.form.speakWho')}</span>
                  <select className="sm-select" value={adv.speakWho} onChange={(e) => { setAdv({ ...adv, speakWho: e.target.value as typeof adv.speakWho }) }}>
                    <option value="all">{t('group.form.speakWho.all')}</option>
                    <option value="owner">{t('group.form.speakWho.owner')}</option>
                    <option value="admins">{t('group.form.speakWho.admins')}</option>
                  </select>
                </label>
                <label className="sm-field">
                  <span>{t('group.form.join')}</span>
                  <select className="sm-select" value={adv.join} onChange={(e) => { setAdv({ ...adv, join: e.target.value as typeof adv.join, ...(e.target.value === 'paid' ? { isPublic: true } : {}) }) }}>
                    <option value="invite">{t('group.form.join.invite')}</option>
                    <option value="apply">{t('group.form.join.apply')}</option>
                    <option value="open">{t('group.form.join.open')}</option>
                    <option value="paid" disabled={walletAddress === undefined}>{t('group.form.join.paid')}{walletAddress === undefined ? `（${t('group.form.join.paid.noWallet')}）` : ''}</option>
                  </select>
                  {adv.join === 'paid' && !adv.isPublic
                    ? <p style={{ fontSize: '0.82em', margin: 0 }}>{t('group.form.join.paid.autoPublic')}</p>
                    : null}
                </label>
                {adv.join === 'paid'
                  ? (
                    <>
                      <label className="sm-field">
                        <span>{t('group.form.joinPrice')}</span>
                        <input className="sm-input" type="text" placeholder="1.00" value={adv.joinPrice} disabled={walletAddress === undefined} onChange={(e) => { setAdv({ ...adv, joinPrice: e.target.value }) }} data-soulmirror-group-create-join-price />
                      </label>
                      {paidIncomplete && adv.joinPrice.trim() === ''
                        ? <p style={{ fontSize: '0.82em', margin: 0, color: 'rgb(220,80,60)' }} data-soulmirror-group-create-paid-missing>{t('group.form.join.paid.priceRequired')}</p>
                        : null}
                      <p style={{ fontSize: '0.82em', opacity: 0.7, margin: 0 }}>
                        {walletAddress === undefined
                          ? t('group.form.join.paid.noWalletHint')
                          : t('group.form.joinAddr', { address: walletAddress })}
                      </p>
                      <p style={{ fontSize: '0.82em', opacity: 0.7, margin: 0 }} data-soulmirror-group-create-paid-hint>
                        {t('group.form.join.paid.membersFree')}
                      </p>
                    </>
                  )
                  : null}
                <label className="sm-field">
                  <span>{t('group.form.wake')}</span>
                  <select className="sm-select" value={adv.agentWake} onChange={(e) => { setAdv({ ...adv, agentWake: e.target.value as typeof adv.agentWake }) }}>
                    <option value="mention">{t('group.form.wake.mention')}</option>
                    <option value="always">{t('group.form.wake.always')}</option>
                    <option value="never">{t('group.form.wake.never')}</option>
                  </select>
                </label>
                <label className="sm-field">
                  <span>{t('group.form.tier')}</span>
                  <select className="sm-select" value={adv.agentTier} onChange={(e) => { setAdv({ ...adv, agentTier: e.target.value as typeof adv.agentTier }) }}>
                    <option value="notify">{t('tier.short.notify')}</option>
                    <option value="draft">{t('tier.short.draft')}</option>
                    <option value="auto">{t('tier.short.auto')}</option>
                  </select>
                </label>
              </div>
              <label className="sm-field">
                <span>{t('group.form.rules')}</span>
                <textarea className="sm-textarea-box" style={{ minHeight: 64 }} value={adv.rules} onChange={(e) => { setAdv({ ...adv, rules: e.target.value }) }} data-soulmirror-group-create-rules />
              </label>
              <label className="sm-checkbox">
                <input type="checkbox" checked={adv.isPublic} onChange={(e) => { setAdv({ ...adv, isPublic: e.target.checked }) }} data-soulmirror-group-create-public />
                {t('group.form.public')}
              </label>
              {adv.isPublic
                ? (
                  <label className="sm-field">
                    <span>{t('group.form.tags')}</span>
                    <input className="sm-input" value={adv.tags} onChange={(e) => { setAdv({ ...adv, tags: e.target.value }) }} data-soulmirror-group-create-tags />
                  </label>
                )
                : null}
            </div>
          )
          : null}
        <div className="sm-modal-foot">
          <Button variant="outline" size="sm" onClick={onClose}>{t('inbox.close')}</Button>
          <Button variant="primary" size="sm" disabled={!ready} onClick={submit} data-soulmirror-group-create-submit>
            {t('group.create.submit')}
          </Button>
        </div>
      </div>
    </div>
  )
}
