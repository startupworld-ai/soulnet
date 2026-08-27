/**
 * Per-GROUP agent settings sheet, opened from the group chat's composer:
 * which of my agents work in THIS group, and who among THIS group's members
 * may command each of them (the whitelist lives here, next to the member
 * list — never on the agent's global definition). The duty picker stays in
 * the composer chips; this sheet is participation + commanders.
 */
import { useState, type CSSProperties } from 'react'
import type { RoomMember } from './group-room.ts'
import type { Translate } from './translate.ts'

const small: CSSProperties = { opacity: 0.75, fontSize: '0.82em', margin: 0 }
const check: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, fontSize: '0.85em' }

type CommanderMode = 'owner' | 'any' | 'custom'

function modeOf(commanders: readonly string[]): CommanderMode {
  if (commanders.includes('*')) return 'any'
  return commanders.length === 0 ? 'owner' : 'custom'
}

export function GroupAgentsSheet({ t, members, myFp, agentNames, voices, commanders, onToggle, onCommanders, onClose }: {
  t: Translate
  members: readonly RoomMember[]
  myFp: string
  /** My agent names (the registry). */
  agentNames: readonly string[]
  /** Voice switches of this group (name → on). */
  voices: Readonly<Record<string, boolean>>
  /** Per-voice commanders of this group (name → fps or '*'). */
  commanders: Readonly<Record<string, readonly string[]>>
  onToggle: (name: string) => void
  onCommanders: (name: string, list: string[]) => void
  onClose: () => void
}) {
  const others = members.filter(m => m.fp !== myFp)
  // 'chosen members' with nothing ticked yet stores the same empty list as
  // 'only me' — remember the picked mode locally so the checkboxes appear.
  const [modeOverride, setModeOverride] = useState<Record<string, CommanderMode>>({})
  return (
    <div
      style={{ position: 'absolute', inset: 0, zIndex: 40, display: 'grid', placeItems: 'center', background: 'rgba(0,0,0,.35)' }}
      role="dialog"
      aria-label={t('group.agents.title')}
      data-soulmirror-modal
      data-soulmirror-group-agents-sheet
      onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}
    >
      <div style={{ width: 'min(440px, calc(100% - 48px))', maxHeight: 'calc(100% - 64px)', overflowY: 'auto', borderRadius: 12, border: '1px solid rgba(127,127,127,.3)', background: 'var(--dsw-specific-menu, var(--dsw-alias-bg-layer-2, #fff))', boxShadow: '0 12px 40px rgba(0,0,0,.28)', padding: 16, display: 'grid', gap: 12 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <strong style={{ flex: 1 }}>{t('group.agents.title')}</strong>
          <button type="button" className="sm-ghostbtn" onClick={onClose}>{t('inbox.close')}</button>
        </div>
        <p style={small}>{t('group.agents.hint')}</p>
        {agentNames.length === 0 ? <p style={small}>{t('group.agents.none')}</p> : null}
        {agentNames.map((name) => {
          const on = voices[name] === true
          const list = commanders[name] ?? []
          const mode = modeOverride[name] ?? modeOf(list)
          const custom = list.filter(c => c !== '*')
          return (
            <div key={name} style={{ border: '1px solid rgba(127,127,127,.25)', borderRadius: 10, padding: '10px 12px', display: 'grid', gap: 8 }} data-soulmirror-group-agent={name}>
              <label style={{ ...check, fontWeight: 600 }}>
                <input type="checkbox" checked={on} onChange={() => { onToggle(name) }} />
                <span>{name}</span>
              </label>
              {on
                ? (
                  <>
                    <span style={{ opacity: 0.8, fontSize: '0.85em' }}>{t('group.agents.commanders')}</span>
                    <select
                      style={{ font: 'inherit', padding: '4px 8px', borderRadius: 6, border: '1px solid rgba(127,127,127,.35)', background: 'transparent', color: 'inherit' }}
                      value={mode}
                      onChange={(e) => {
                        const next = e.target.value as CommanderMode
                        setModeOverride(prev => ({ ...prev, [name]: next }))
                        onCommanders(name, next === 'any' ? ['*'] : next === 'owner' ? [] : custom)
                      }}
                      data-soulmirror-group-agent-mode
                    >
                      <option value="owner">{t('settings.agents.commanders.ownerOnly')}</option>
                      <option value="any">{t('settings.agents.commanders.any')}</option>
                      <option value="custom">{t('group.agents.commanders.custom')}</option>
                    </select>
                    {mode === 'custom' && others.map(m => (
                      <label key={m.fp} style={check}>
                        <input
                          type="checkbox"
                          checked={custom.includes(m.fp)}
                          onChange={(e) => { onCommanders(name, e.target.checked ? [...custom, m.fp] : custom.filter(c => c !== m.fp)) }}
                        />
                        <span>{m.name}</span>
                      </label>
                    ))}
                    {mode === 'custom' && others.length === 0 ? <p style={small}>{t('settings.agents.commanders.ownerOnly')}</p> : null}
                  </>
                )
                : null}
            </div>
          )
        })}
      </div>
    </div>
  )
}
