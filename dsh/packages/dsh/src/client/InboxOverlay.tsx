/**
 * `shell.overlay` entry `soulmirror-inbox`: the new-mail cue — a ui-primitives
 * Toast "New message from <name>" for mail whose session is not the one on
 * screen and whose thread is not open on the SoulMirror page. The memory
 * extraction popup lives in ./MemoryNotification.tsx (mounted here so the SSE
 * stream stays open for its own subscription too). Always mounted (the overlay
 * layer is click-through until an entry renders something).
 */
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import { networkStore } from './api.ts'
import { shouldNotify, type MailNotice } from './inbox-state.ts'
import type { NS } from './locales.ts'
import { MemoryNotification } from './MemoryNotification.tsx'
import { groupKey } from './page-state.ts'
import { pageStore } from './page-store.ts'
import { SoulMirrorIcon } from './SidebarEntry.tsx'

export interface InboxOverlayInjected {
  /** The session currently on screen (ctx.sessions.list current), for the new-mail cue. */
  currentSessionId: () => string | undefined
}

export type InboxOverlayProps = InboxOverlayInjected & PropsLocale<typeof NS>

const MAX_TOASTS = 3

interface ToastEntry extends MailNotice { key: number }

export function InboxOverlay({ currentSessionId, t }: InboxOverlayProps) {
  // Subscribing keeps the SSE stream open for the whole page lifetime.
  useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const [toasts, setToasts] = useState<ToastEntry[]>([])
  const seq = useRef(0)
  useEffect(() => networkStore.onMail((notice) => {
    const page = pageStore.getSnapshot()
    // The thread key of the notice: a group message belongs to its group
    // conversation (fp is the sender there), a DM to the friend thread.
    const threadKey = notice.gid !== undefined ? groupKey(notice.gid) : notice.fp
    if (page.open && page.selected === threadKey) return
    // do-not-disturb: muted friend / muted group
    const inbox = networkStore.getSnapshot().inbox
    const muted = notice.gid !== undefined
      ? inbox.groups.find(g => g.gid === notice.gid)?.muted === true
      : inbox.friends.find(f => f.fp === notice.fp)?.muted === true
    if (muted) return
    if (!shouldNotify(notice, currentSessionId())) return
    seq.current += 1
    const entry: ToastEntry = { ...notice, key: seq.current }
    setToasts(prev => [...prev.slice(-(MAX_TOASTS - 1)), entry])
  }), [currentSessionId])
  const dismiss = useCallback((key: number) => { setToasts(prev => prev.filter(x => x.key !== key)) }, [])

  return (
    <>
      <MemoryNotification t={t} />
      {toasts.map(toast => (
        <MailToast key={toast.key} entry={toast} t={t} onDone={() => { dismiss(toast.key) }} />
      ))}
    </>
  )
}

function MailToast({ entry, t, onDone }: { entry: ToastEntry; t: InboxOverlayProps['t']; onDone: () => void }) {
  // Own themed pill instead of the host Toast (whose surface stays light on
  // the dark theme); auto-dismisses, click dismisses early.
  useEffect(() => {
    const timer = setTimeout(onDone, 4_500)
    return () => { clearTimeout(timer) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  return (
    <button type="button" className="sm-mail-toast" onClick={onDone} data-soulmirror-mail-toast>
      <SoulMirrorIcon size={16} />
      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{t('toast.newMail', { name: entry.name })}</span>
    </button>
  )
}
