/**
 * Folding rules of the group room's visible timeline
 * (src/client/rooms/timeline.ts `buildTimeline`): day separators, sender-run
 * folding by (sender × voice), the owner's right-side messages resetting the
 * run, and work traces opening with a `work-head` title the voice's own post
 * folds under.
 */
import { describe, expect, it } from 'vitest'
import type { ApiChatItem } from '../src/client/api.ts'
import type { ThreadEntry } from '../src/client/page-state.ts'
import { buildTimeline, RUN_GAP_MS } from '../src/client/rooms/timeline.ts'

// A fixed noon so ts offsets never cross a day boundary by accident.
const NOON = new Date(2026, 7, 24, 12, 0, 0).getTime()

let n = 0
const entry = (over: Partial<ThreadEntry> & { ts: number }): ThreadEntry => {
  n += 1
  return { seq: n, dir: 'in', id: `m${n}`, body: `body ${n}`, from: 'fp-alice', ...over }
}
const kinds = (rows: ReturnType<typeof buildTimeline>): string[] => rows.map(r => r.kind)
const headers = (rows: ReturnType<typeof buildTimeline>): boolean[] =>
  rows.filter(r => r.kind === 'entry').map(r => (r as { showHeader: boolean }).showHeader)

describe('buildTimeline', () => {
  it('separates days and folds one sender into a run (header on the first row only)', () => {
    const rows = buildTimeline([
      entry({ ts: NOON }),
      entry({ ts: NOON + 1000 }),
      entry({ ts: NOON + 24 * 60 * 60_000 }),
    ], {})
    expect(kinds(rows)).toEqual(['day', 'entry', 'entry', 'day', 'entry'])
    // The new day resets the run: its first entry shows the header again.
    expect(headers(rows)).toEqual([true, false, true])
  })

  it('reopens the header after the run gap', () => {
    const rows = buildTimeline([
      entry({ ts: NOON }),
      entry({ ts: NOON + RUN_GAP_MS + 1 }),
    ], {})
    expect(headers(rows)).toEqual([true, true])
  })

  it('keys runs by (sender × voice): a member and their agent never fold together', () => {
    const rows = buildTimeline([
      entry({ ts: NOON, from: 'fp-alice' }),
      entry({ ts: NOON + 1000, from: 'fp-alice', by: 'alter', agent: 'AliceBot' }),
      entry({ ts: NOON + 2000, from: 'fp-alice', by: 'alter', agent: 'AliceBot' }),
      entry({ ts: NOON + 3000, from: 'fp-alice' }),
    ], {})
    expect(headers(rows)).toEqual([true, true, false, true])
  })

  it("the owner's typed message resets the run and never carries a header", () => {
    const rows = buildTimeline([
      entry({ ts: NOON }),
      entry({ ts: NOON + 1000, dir: 'out' }),
      entry({ ts: NOON + 2000 }),
    ], {})
    expect(headers(rows)).toEqual([true, false, true])
  })

  it('a work burst opens with one title; the voice post folds under it; empty notes are skipped', () => {
    const items: ApiChatItem[] = [
      { kind: 'thinking', key: 't1', ts: NOON + 100, text: 'hm' },
      { kind: 'tool', key: 'o1', ts: NOON + 200, name: 'read', args: '{}' },
      { kind: 'alter', key: 'a1', ts: NOON + 250, text: '   ', turn: 1 },
      { kind: 'alter', key: 'a2', ts: NOON + 400, text: 'private note', turn: 1 },
      { kind: 'turn-failed', key: 'f1', ts: NOON + 500, turn: 1, reason: 'boom' },
    ]
    const rows = buildTimeline([
      entry({ ts: NOON + 300, dir: 'out', by: 'alter', agent: 'DevBot' }),
    ], { DevBot: { items } })
    expect(kinds(rows)).toEqual(['day', 'work-head', 'process', 'process', 'entry', 'note', 'work-failed'])
    // The voice's own post continues the work run: no second header.
    expect(headers(rows)).toEqual([false])
    expect(rows.filter(r => r.kind === 'work-head')).toHaveLength(1)
  })

  it('a second burst after the gap gets a fresh title', () => {
    const items: ApiChatItem[] = [
      { kind: 'thinking', key: 't1', ts: NOON, text: 'first' },
      { kind: 'thinking', key: 't2', ts: NOON + RUN_GAP_MS + 1, text: 'second' },
    ]
    const rows = buildTimeline([], { DevBot: { items } })
    expect(kinds(rows)).toEqual(['day', 'work-head', 'process', 'work-head', 'process'])
  })

  it('two agents interleaving each keep their own titled runs', () => {
    const rows = buildTimeline([], {
      DevBot: { items: [{ kind: 'thinking', key: 'd1', ts: NOON, text: 'a' }] },
      OpsBot: { items: [{ kind: 'thinking', key: 'o1', ts: NOON + 1000, text: 'b' }] },
    })
    expect(rows.filter(r => r.kind === 'work-head').map(r => (r as { agent: string }).agent)).toEqual(['DevBot', 'OpsBot'])
  })
})

describe('clock skew (archive order wins)', () => {
  it('a later-arrived message with an EARLIER sender ts stays below what is on screen', () => {
    // seq order: their "2" (fast clock) arrived, then my "3" (slow clock, ts 8s earlier).
    const rows = buildTimeline([
      entry({ ts: NOON, from: 'fp-alice' }),                      // "1"
      entry({ ts: NOON + 10_000, from: 'fp-alice' }),             // "2" - sender clock runs fast
      entry({ ts: NOON + 2_000, dir: 'out' }),                    // "3" - my slower clock
    ], {})
    const bodies = rows.filter(r => r.kind === 'entry').map(r => (r as { entry: { body: string } }).entry.body)
    expect(bodies).toEqual(bodies.slice().sort((a, b) => a.localeCompare(b))) // body 1,2,3 in seq order
    expect(rows.map(r => r.kind)).toEqual(['day', 'entry', 'entry', 'entry'])
  })

  it('a work trace still interleaves by its local time between clamped entries', () => {
    const rows = buildTimeline([
      entry({ ts: NOON }),
      entry({ ts: NOON + 60_000 }),
    ], { DevBot: { items: [{ kind: 'thinking', key: 't1', ts: NOON + 30_000, text: 'between' }] } })
    expect(rows.map(r => r.kind)).toEqual(['day', 'entry', 'work-head', 'process', 'entry'])
  })

  it('a sender ts from yesterday cannot drag the day separator backwards', () => {
    const rows = buildTimeline([
      entry({ ts: NOON }),
      entry({ ts: NOON - 24 * 60 * 60_000 }), // absurd skew: claims yesterday
    ], {})
    expect(rows.filter(r => r.kind === 'day')).toHaveLength(1)
  })
})
