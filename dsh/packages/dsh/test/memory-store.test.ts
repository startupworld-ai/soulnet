/**
 * Unit tests for the memory store (src/memory/store.ts): schema, bigram
 * segmentation, scope isolation, dedup, and removal.
 */
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { MemoryStore, segment } from '../src/memory/store.ts'

describe('segment', () => {
  it('bigrams CJK runs and keeps words (lowercased)', () => {
    expect(segment('owner 喜欢打篮球')).toBe('owner 喜欢 欢打 打篮 篮球')
    expect(segment('篮球')).toBe('篮球')
    expect(segment('喜欢')).toBe('喜欢')
    expect(segment('AB')).toBe('ab')
    expect(segment('')).toBe('')
  })
})

describe('MemoryStore', () => {
  let dir: string
  let store: MemoryStore

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'soulnet-memory-'))
    store = new MemoryStore(join(dir, 'dsh-memory.db'))
  })

  afterEach(() => {
    store.close()
    rmSync(dir, { recursive: true, force: true })
  })

  it('creates the schema (memories + FTS5 + embedding column)', () => {
    const cols = (store as unknown as { db: { prepare(s: string): { all(): Array<{ name: string }> } } }).db
      .prepare('PRAGMA table_info(memories)').all().map(r => r.name)
    expect(cols).toContain('embedding')
    expect(cols).toContain('scope_kind')
    expect(cols).toContain('scope_key')
  })

  it('migrates a pre-origin database by adding the origin column', () => {
    // Build an old-schema db (no `origin` column) in its own temp dir, then reopen
    // it through MemoryStore — independent of the beforeEach store.
    const legacyDir = mkdtempSync(join(tmpdir(), 'soulnet-memory-legacy-'))
    try {
      const legacy = new DatabaseSync(join(legacyDir, 'dsh-memory.db'))
      legacy.exec(`
        CREATE TABLE memories (
          id          INTEGER PRIMARY KEY AUTOINCREMENT,
          uid         TEXT NOT NULL UNIQUE,
          kind        TEXT NOT NULL,
          content     TEXT NOT NULL,
          scope_kind  TEXT NOT NULL,
          scope_key   TEXT,
          source_ch   TEXT NOT NULL,
          source_ref  TEXT NOT NULL DEFAULT '',
          weight      REAL NOT NULL DEFAULT 1.0,
          created_at  INTEGER NOT NULL,
          last_hit_at INTEGER,
          hit_count   INTEGER NOT NULL DEFAULT 0,
          embedding   BLOB
        );
        INSERT INTO memories (uid, kind, content, scope_kind, source_ch, created_at)
        VALUES ('legacy-uid', 'fact', '旧记忆', 'global', 'alter', 1);
      `)
      legacy.close()

      const migrated = new MemoryStore(join(legacyDir, 'dsh-memory.db'))
      const cols = (migrated as unknown as { db: { prepare(s: string): { all(): Array<{ name: string }> } } }).db
        .prepare('PRAGMA table_info(memories)').all().map(r => r.name)
      expect(cols).toContain('origin')
      // The existing row reads back with the default origin (auto) — no data loss.
      expect(migrated.get('legacy-uid')?.origin).toBe('auto')
      migrated.close()
    } finally {
      rmSync(legacyDir, { recursive: true, force: true })
    }
  })

  it('adds, dedups identical content+scope, and counts', () => {
    const a = store.add({ kind: 'fact', content: 'owner 喜欢打篮球', scope: { kind: 'global' }, sourceCh: 'alter' })
    const b = store.add({ kind: 'fact', content: 'owner 喜欢打篮球', scope: { kind: 'global' }, sourceCh: 'alter' })
    expect(b.id).toBe(a.id)
    expect(store.count({ global: true })).toBe(1)
  })

  it('retrieves by Chinese keyword (bigram matching)', () => {
    store.add({ kind: 'fact', content: 'owner 喜欢打篮球', scope: { kind: 'global' }, sourceCh: 'alter' })
    store.add({ kind: 'preference', content: '投篮训练用右手', scope: { kind: 'agent', name: 'basketball' }, sourceCh: 'agent' })
    const hits = store.retrieve({ global: true }, '篮球', 5)
    expect(hits.map(h => h.content)).toContain('owner 喜欢打篮球')
  })

  it('retrieves all in-scope memories on an empty query (no FTS5 "match all" error)', () => {
    store.add({ kind: 'fact', content: 'owner 喜欢打篮球', scope: { kind: 'global' }, sourceCh: 'alter' })
    store.add({ kind: 'preference', content: '投篮训练用右手', scope: { kind: 'agent', name: 'basketball' }, sourceCh: 'agent' })
    // Empty query (direct owner turn) must NOT throw "unknown special query" and
    // must return both the global memory and the agent's own memory.
    const hits = store.retrieve({ global: true, agent: 'basketball' }, '', 10)
    expect(hits.map(h => h.content).sort()).toEqual(['owner 喜欢打篮球', '投篮训练用右手'])
    // Pure punctuation (segments to empty) behaves the same.
    expect(store.retrieve({ global: true }, '，。！', 10).map(h => h.content)).toContain('owner 喜欢打篮球')
  })

  it('isolates scopes: an agent sees global + its own memory only', () => {
    store.add({ kind: 'fact', content: 'owner 喜欢打篮球', scope: { kind: 'global' }, sourceCh: 'alter' })
    store.add({ kind: 'fact', content: '篮球用右手投篮', scope: { kind: 'agent', name: 'basketball' }, sourceCh: 'agent' })
    store.add({ kind: 'fact', content: '工作负责 soulnet 前端', scope: { kind: 'agent', name: 'work' }, sourceCh: 'agent' })

    const b = store.retrieve({ global: true, agent: 'basketball' }, '篮球', 10)
    expect(b.map(x => x.content)).toContain('篮球用右手投篮')
    expect(b.map(x => x.content)).toContain('owner 喜欢打篮球')
    expect(b.map(x => x.content)).not.toContain('工作负责 soulnet 前端')

    const w = store.retrieve({ global: true, agent: 'work' }, '篮球', 10)
    expect(w.map(x => x.content)).not.toContain('篮球用右手投篮')
    expect(w.map(x => x.content)).toContain('owner 喜欢打篮球')
  })

  it('separates shared-friend and shared-group scopes', () => {
    store.add({ kind: 'fact', content: '给 alice 的共享记忆', scope: { kind: 'shared-friend', fp: 'fp-alice' }, sourceCh: 'alter' })
    store.add({ kind: 'fact', content: '给群 g1 的共享记忆', scope: { kind: 'shared-group', gid: 'g1' }, sourceCh: 'alter' })

    const f = store.retrieve({ global: true, friend: 'fp-alice' }, '共享', 10)
    expect(f.map(x => x.content)).toContain('给 alice 的共享记忆')
    expect(f.map(x => x.content)).not.toContain('给群 g1 的共享记忆')

    const g = store.retrieve({ global: true, group: 'g1' }, '共享', 10)
    expect(g.map(x => x.content)).toContain('给群 g1 的共享记忆')
    expect(g.map(x => x.content)).not.toContain('给 alice 的共享记忆')
  })

  it('removes, and an empty allow returns nothing', () => {
    const a = store.add({ kind: 'fact', content: 'x', scope: { kind: 'global' }, sourceCh: 'alter' })
    expect(store.remove(a.uid)).toBe(true)
    expect(store.remove('missing')).toBe(false)
    expect(store.count({ global: true })).toBe(0)
    expect(store.retrieve({}, 'x', 5)).toEqual([])
    expect(store.list({})).toEqual([])
  })
})
