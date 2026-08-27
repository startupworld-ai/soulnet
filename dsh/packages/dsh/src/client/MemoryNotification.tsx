/**
 * Memory-extraction popup (product parity with the lingshu reference):
 * every turn ends with a "distilling" notice that steps through a simulated
 * progress, then shows the extracted memories with a countdown that KEEPS them
 * by default, or a short "nothing to save" notice when extraction came back
 * empty. Keeping shows a transient guide bubble.
 */
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import { api, networkStore } from './api.ts'
import type { NS } from './locales.ts'
import { pageStore } from './page-store.ts'

type Phase = 'processing' | 'result' | 'empty' | 'guide'

interface Memory { id: string; content: string }

interface Task {
  phase: Phase
  memories: Memory[]
  remaining: number
  step: number
  count: number
  clue: string
}

const KEEP_SECONDS = 5
const EMPTY_SECONDS = 3
const GUIDE_MS = 5000
const STEP_DELAY_MS = 1100

export type MemoryNotificationProps = PropsLocale<typeof NS>

const box: React.CSSProperties = {
  position: 'fixed',
  top: 84,
  right: 24,
  width: 320,
  maxWidth: 'calc(100vw - 48px)',
  zIndex: 2000,
  background: 'var(--dsw-alias-bg-layer-2, var(--dsw-alias-bg-base))',
  border: '1px solid var(--dsw-alias-border, rgba(128,128,128,0.3))',
  borderRadius: 12,
  boxShadow: '0 8px 32px rgba(0,0,0,0.28)',
  padding: 16,
  color: 'var(--dsw-alias-label-primary, #e6e6e6)',
  fontFamily: 'inherit',
}

const title: React.CSSProperties = { margin: 0, fontSize: 14, fontWeight: 600 }
const hint: React.CSSProperties = { marginTop: 6, fontSize: 12.5, lineHeight: 1.55, opacity: 0.82 }
const list: React.CSSProperties = { marginTop: 10, display: 'flex', flexDirection: 'column', gap: 6 }
const stepRow: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, fontSize: 12.5, lineHeight: 1.4 }
const dot: React.CSSProperties = { width: 6, height: 6, borderRadius: 3, flexShrink: 0 }
const dotLoading: React.CSSProperties = { ...dot, background: 'var(--dsw-alias-accent, #4c8dff)', animation: 'soulnet-pulse 1.2s ease-in-out infinite' }
const dotDone: React.CSSProperties = { ...dot, background: 'var(--dsw-alias-state-success-primary, #4ade80)' }
const points: React.CSSProperties = { marginTop: 8, display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 200, overflowY: 'auto' }
const point: React.CSSProperties = { fontSize: 12.5, lineHeight: 1.5, padding: '7px 10px', borderRadius: 8, background: 'var(--dsw-alias-bg-layer-1, rgba(255,255,255,0.04))' }
const actions: React.CSSProperties = { marginTop: 12, display: 'flex', gap: 8, justifyContent: 'flex-end' }
const ghostBtn: React.CSSProperties = { padding: '6px 12px', fontSize: 12.5, borderRadius: 8, cursor: 'pointer', border: '1px solid var(--dsw-alias-border, rgba(128,128,128,0.4))', background: 'transparent', color: 'inherit' }
const primaryBtn: React.CSSProperties = { padding: '6px 12px', fontSize: 12.5, borderRadius: 8, cursor: 'pointer', border: 'none', background: 'var(--dsw-alias-accent, #4c8dff)', color: '#fff' }
const guideBox: React.CSSProperties = { ...box, width: 300, top: 84, right: 24 }
const guideCopy: React.CSSProperties = { fontSize: 13, fontWeight: 600 }
const guideSub: React.CSSProperties = { marginTop: 6, fontSize: 12, lineHeight: 1.5, opacity: 0.8 }

export function MemoryNotification({ t }: MemoryNotificationProps) {
  const [task, setTask] = useState<Task | null>(null)
  const timersRef = useRef<ReturnType<typeof setTimeout>[]>([])
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const clearAll = useCallback(() => {
    timersRef.current.forEach(clearTimeout)
    timersRef.current = []
    if (intervalRef.current !== null) {
      clearInterval(intervalRef.current)
      intervalRef.current = null
    }
  }, [])

  const startCountdown = useCallback((seconds: number, onZero: () => void) => {
    intervalRef.current = setInterval(() => {
      setTask(prev => {
        if (prev === null) return prev
        const next = prev.remaining - 1
        if (next > 0) return { ...prev, remaining: next }
        if (intervalRef.current !== null) clearInterval(intervalRef.current)
        intervalRef.current = null
        onZero()
        return prev
      })
    }, 1000)
  }, [])

  useEffect(() => networkStore.onMemory((frame) => {
    clearAll()
    if (frame.phase === 'extracting') {
      setTask({ phase: 'processing', memories: [], remaining: KEEP_SECONDS, step: 0, count: 0, clue: frame.clue ?? '' })
      const stepTimer = setTimeout(() => {
        setTask(prev => (prev !== null && prev.phase === 'processing' ? { ...prev, step: 1 } : prev))
      }, STEP_DELAY_MS)
      timersRef.current.push(stepTimer)
      return
    }
    // extracted
    const memories = frame.memories ?? []
    const count = frame.count
    if (memories.length > 0) {
      setTask({ phase: 'result', memories, remaining: KEEP_SECONDS, step: 2, count, clue: frame.clue ?? '' })
      startCountdown(KEEP_SECONDS, () => {
        // default keep: show the guide bubble, then clear
        setTask(prev => (prev === null ? prev : { ...prev, phase: 'guide' }))
        const guideTimer = setTimeout(() => setTask(null), GUIDE_MS)
        timersRef.current.push(guideTimer)
      })
    } else {
      setTask({ phase: 'empty', memories: [], remaining: EMPTY_SECONDS, step: 2, count, clue: frame.clue ?? '' })
      startCountdown(EMPTY_SECONDS, () => setTask(null))
    }
  }), [clearAll, startCountdown])

  // The notice belongs to the turn of the session that was on screen when it
  // fired: switching sessions (or closing the page) ends it at once instead of
  // leaving a stale "distilling" bubble floating over a different conversation.
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  useEffect(() => {
    clearAll()
    setTask(null)
  }, [page.selected, page.open, clearAll])

  const onCancel = useCallback(() => {
    const ids = task?.memories.map(m => m.id).filter(Boolean) ?? []
    void api.cancelMemory(ids).catch(() => {})
    clearAll()
    setTask(null)
  }, [task, clearAll])

  const onClose = useCallback(() => {
    clearAll()
    setTask(null)
  }, [clearAll])

  if (task === null) return null

  if (task.phase === 'guide') {
    return (
      <div style={guideBox} aria-live="polite" data-soulmirror-float>
        <div style={guideCopy}>{t('memory.guide', { count: task.count, plural: task.count === 1 ? 'y' : 'ies' })}</div>
        <div style={guideSub}>{t('memory.guide.sub')}</div>
      </div>
    )
  }

  return (
    <div style={box} aria-live="polite" data-soulmirror-float>
      <div style={title}>{t('memory.title')}</div>

      {task.phase === 'processing' ? (
        <>
          <div style={hint}>{t('memory.processing.hint')}</div>
          <div style={list}>
            <div style={stepRow}>
              <span style={task.step >= 0 ? dotDone : dotLoading} />
              <span>{t('memory.step.clue', { summary: task.clue === '' ? '…' : task.clue })}</span>
            </div>
            <div style={stepRow}>
              <span style={task.step >= 1 ? dotLoading : dot} />
              <span>{t('memory.step.semantic')}</span>
            </div>
          </div>
        </>
      ) : task.phase === 'result' ? (
        <>
          <div style={hint}>{t('memory.result.hint')}</div>
          <div style={{ marginTop: 10, fontSize: 12.5, fontWeight: 600 }}>{t('memory.result.title')}</div>
          <div style={points}>
            {task.memories.map(m => (
              <div key={m.id} style={point}>{m.content}</div>
            ))}
          </div>
          <div style={actions}>
            <button type="button" style={ghostBtn} onClick={onCancel}>{t('memory.cancel')}</button>
            <button type="button" style={primaryBtn} onClick={() => {
              // explicit keep = same as countdown expiry
              clearAll()
              setTask(prev => (prev === null ? prev : { ...prev, phase: 'guide' }))
              const guideTimer = setTimeout(() => setTask(null), GUIDE_MS)
              timersRef.current.push(guideTimer)
            }}>{t('memory.keep', { n: task.remaining })}</button>
          </div>
        </>
      ) : (
        <>
          <div style={hint}>{t('memory.empty.hint')}</div>
          <div style={{ marginTop: 10, fontSize: 12.5, fontWeight: 600 }}>{t('memory.empty.title')}</div>
          <div style={actions}>
            <button type="button" style={ghostBtn} onClick={onClose}>{t('memory.empty.gotIt', { n: task.remaining })}</button>
            <button type="button" style={primaryBtn} onClick={onClose}>{t('memory.empty.continue')}</button>
          </div>
        </>
      )}
    </div>
  )
}
