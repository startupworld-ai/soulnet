/**
 * `shell.overlay` entry `soulmirror-inbox`: the new-mail cue — a ui-primitives
 * Toast "New message from <name>" for mail whose session is not the one on
 * screen and whose thread is not open on the SoulMirror page. The memory
 * extraction popup lives in ./MemoryNotification.tsx (mounted here so the SSE
 * stream stays open for its own subscription too). Always mounted (the overlay
 * layer is click-through until an entry renders something).
 */
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { Toast } from '@deepseek-ai/dsh-client-ui-primitives'
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
    const inbox = networkStore.getSnapshot().inbox
    if (notice.gid !== undefined) {
      // Group mail: suppress when that group is open on the page or muted.
      if (page.open && page.selected === groupKey(notice.gid)) return
      const group = inbox.groups.find(g => g.gid === notice.gid)
      if (group?.muted === true) return
    } else {
      if (page.open && page.selected === notice.fp) return
      const friend = inbox.friends.find(f => f.fp === notice.fp)
      if (friend?.muted === true) return // do-not-disturb
    }
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
  return <Toast text={t('toast.newMail', { name: entry.name })} icon={<SoulMirrorIcon size={16} />} onDone={onDone} />
}
