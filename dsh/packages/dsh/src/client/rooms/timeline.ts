/**
 * Pure assembly of the group room's visible timeline: archive entries MERGED
 * with my agents' local work traces (thinking / tools / private notes),
 * ordered by time. Extracted from ChatRoom.tsx so the folding rules are unit
 * testable; the component only maps rows to elements.
 *
 * Folding rules:
 *   - a day separator opens every new calendar day (and resets the run);
 *   - sender runs group by (sender × voice) — a member's agent is its OWN
 *     speaker, never folded into that member's human messages — and only the
 *     first row of a run shows the avatar/name header;
 *   - the owner's typed messages (right side) reset the run and never carry
 *     a header;
 *   - work traces join the SAME run as the voice's own posts: the first row
 *     of a work burst announces WHO started working (the `work-head` title),
 *     and the voice's group post that follows folds under it without a
 *     second header.
 */
import type { ApiChatItem } from '../api.ts'
import { dayKey, type ThreadEntry } from '../page-state.ts'
import type { ProcessItem } from '../process-ui.tsx'

/** Two messages of one sender within this gap fold into one run (no repeated name/avatar). */
export const RUN_GAP_MS = 5 * 60_000

export type TimelineRow =
  | { kind: 'day'; key: string; ts: number }
  | { kind: 'entry'; key: string; entry: ThreadEntry; showHeader: boolean }
  | { kind: 'work-head'; key: string; ts: number; agent: string }
  | { kind: 'process'; key: string; ts: number; item: ProcessItem }
  | { kind: 'note'; key: string; ts: number; agent: string; text: string }
  | { kind: 'work-failed'; key: string; ts: number; agent: string; reason: string }

export function buildTimeline(
  entries: readonly ThreadEntry[],
  workFeeds: Readonly<Record<string, { readonly items: readonly ApiChatItem[] }>>,
): TimelineRow[] {
  // Entries KEEP their archive order (arrival/seq - sortThread's contract).
  // Their ts is the SENDER's clock, and peers' clocks skew: re-sorting by ts
  // would let a just-sent message jump ABOVE one already on screen. Instead
  // each entry gets a display time clamped to be monotonic (never before the
  // previous entry), used only for interleaving work traces and for the day
  // separators - the bubble still shows the sender's own clock.
  let clamp = 0
  const entrySlots = entries.map((e) => {
    clamp = Math.max(e.ts, clamp + 1)
    return { ts: clamp, entry: e }
  })
  const mergedSlots: { ts: number; entry?: ThreadEntry; agent?: string; item?: ApiChatItem }[] = [
    ...entrySlots,
    ...Object.entries(workFeeds).flatMap(([agent, f]) => f.items.map(item => ({ ts: item.ts, agent, item }))),
  ].sort((a, b) => a.ts - b.ts)
  const timeline: TimelineRow[] = []
  let lastDay: string | undefined
  let runFrom: string | undefined
  let runTs = 0
  for (const slot of mergedSlots) {
    const day = dayKey(slot.ts)
    if (day !== lastDay) {
      timeline.push({ kind: 'day', key: `day:${day}`, ts: slot.ts })
      lastDay = day
      runFrom = undefined
    }
    if (slot.entry !== undefined) {
      const e = slot.entry
      const senderKey = e.by === 'alter'
        ? `voice:${e.dir === 'out' ? 'me' : (e.from ?? '')}:${e.agent ?? 'alter'}`
        : e.dir === 'out' ? undefined : e.from
      let showHeader = false
      if (senderKey === undefined) runFrom = undefined
      else {
        showHeader = senderKey !== runFrom || slot.ts - runTs > RUN_GAP_MS
        runFrom = senderKey
        runTs = slot.ts
      }
      timeline.push({ kind: 'entry', key: e.clientId ?? (e.seq > 0 ? `seq:${e.seq}` : `id:${e.id}`), entry: e, showHeader })
      continue
    }
    const agent = slot.agent!
    const item = slot.item!
    let row: TimelineRow | undefined
    if (item.kind === 'thinking' || item.kind === 'tool') row = { kind: 'process', key: `p:${agent}:${item.key}`, ts: slot.ts, item }
    else if (item.kind === 'alter' && item.text.trim() !== '') row = { kind: 'note', key: `n:${agent}:${item.key}`, ts: slot.ts, agent, text: item.text }
    else if (item.kind === 'turn-failed') row = { kind: 'work-failed', key: `f:${agent}:${item.key}`, ts: slot.ts, agent, reason: item.message ?? item.reason }
    if (row === undefined) continue
    const wKey = `voice:me:${agent}`
    if (wKey !== runFrom || slot.ts - runTs > RUN_GAP_MS) {
      timeline.push({ kind: 'work-head', key: `wh:${agent}:${item.key}`, ts: slot.ts, agent })
    }
    runFrom = wKey
    runTs = slot.ts
    timeline.push(row)
  }
  return timeline
}
