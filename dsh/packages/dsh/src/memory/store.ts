/**
 * 分身记忆存储（P1 轻量版）：单文件 SQLite + FTS5 全文检索。
 *
 * 三张表：
 *   - memories      记忆主表（元数据 + 正文 + scope 权限 + embedding 向量预留列）
 *   - memories_fts  FTS5 全文索引（应用层预分词，uid 关联主表）
 *   - （P2）向量检索：memories.embedding 列已预留，届时用 sqlite-vec 或 HNSW 索引填充；本期不启用
 *
 * scope 用 (scope_kind, scope_key) 两列表达，不搞一堆布尔列：
 *   global / agent:<name> / shared-friend:<fp> / shared-group:<gid>
 *
 * 中文检索：FTS5 默认 tokenizer 对中文（无空格）不友好，这里在写入/查询时
 * 用「字符 bigram」预分词——中文连续段拆成相邻两字，"篮球" 能命中"打篮球"，
 * 英文/数字按词保留。零外部依赖，SQLite 内置能力。
 *
 * 检索原则：永不整库注入，只按 scope 过滤 + 关键词 top-K 召回。
 */
import { DatabaseSync } from 'node:sqlite'
import { randomUUID } from 'node:crypto'
import { mkdirSync } from 'node:fs'
import { dirname } from 'node:path'

export type MemoryKind = 'fact' | 'preference' | 'summary' | 'decision' | 'promise'

export type MemoryScope =
  | { kind: 'global' }
  | { kind: 'agent'; name: string }
  | { kind: 'shared-friend'; fp: string }
  | { kind: 'shared-group'; gid: string }

export interface NewMemory {
  kind: MemoryKind
  content: string
  scope: MemoryScope
  sourceCh: string
  sourceRef?: string
  weight?: number
  /** 'auto' = extracted from a conversation; 'manual' = the owner added it by hand. Defaults to 'auto'. */
  origin?: 'auto' | 'manual'
}

export interface MemoryRecord {
  id: number
  uid: string
  kind: MemoryKind
  content: string
  scope: MemoryScope
  sourceCh: string
  sourceRef: string
  weight: number
  origin: 'auto' | 'manual'
  createdAt: number
  lastHitAt: number | null
  hitCount: number
}

/** 允许召回哪些 scope（缺省一律不允许，避免越权读到别的分身的记忆）。 */
export interface AllowScopes {
  global?: boolean
  agent?: string
  friend?: string
  group?: string
}

/**
 * 中文友好的预分词：中文连续段 → 字符 bigram；英文/数字按词保留（小写）。
 * "owner 喜欢打篮球" → "owner 喜欢 欢打 打篮 篮球"；查询端用同一函数。
 */
export function segment(text: string): string {
  const out: string[] = []
  const flushCjk = (run: string): void => {
    if (run.length === 1) out.push(run)
    else for (let i = 0; i < run.length - 1; i++) out.push(run.slice(i, i + 2))
  }
  const flushWord = (run: string): void => {
    if (run !== '') out.push(run.toLowerCase())
  }
  let cjk = ''
  let word = ''
  for (const ch of text) {
    if (/[\u4e00-\u9fff]/.test(ch)) {
      flushWord(word); word = ''
      cjk += ch
    } else if (/[a-zA-Z0-9]/.test(ch)) {
      flushCjk(cjk); cjk = ''
      word += ch
    } else {
      flushWord(word); flushCjk(cjk); word = ''; cjk = ''
    }
  }
  flushWord(word); flushCjk(cjk)
  return out.join(' ')
}

function scopeKind(scope: MemoryScope): string {
  return scope.kind
}

function scopeKey(scope: MemoryScope): string | null {
  switch (scope.kind) {
    case 'global': return null
    case 'agent': return scope.name
    case 'shared-friend': return scope.fp
    case 'shared-group': return scope.gid
  }
}

function decodeScope(kind: string, key: string | null): MemoryScope {
  switch (kind) {
    case 'global': return { kind: 'global' }
    case 'agent': return { kind: 'agent', name: key ?? '' }
    case 'shared-friend': return { kind: 'shared-friend', fp: key ?? '' }
    case 'shared-group': return { kind: 'shared-group', gid: key ?? '' }
    default: return { kind: 'global' }
  }
}

function rowToRecord(row: Record<string, unknown>): MemoryRecord {
  return {
    id: row.id as number,
    uid: row.uid as string,
    kind: row.kind as MemoryKind,
    content: row.content as string,
    scope: decodeScope(row.scope_kind as string, (row.scope_key as string | null) ?? null),
    sourceCh: row.source_ch as string,
    sourceRef: (row.source_ref as string) ?? '',
    weight: row.weight as number,
    origin: row.origin === 'manual' ? 'manual' : 'auto',
    createdAt: row.created_at as number,
    lastHitAt: (row.last_hit_at as number | null) ?? null,
    hitCount: row.hit_count as number,
  }
}

/** 组装 scope 过滤的 WHERE 片段；空数组 = 无任何可见记忆。 */
function scopeClause(allow: AllowScopes): { sql: string; params: string[] } {
  const where: string[] = []
  const params: string[] = []
  if (allow.global) where.push("scope_kind = 'global'")
  if (allow.agent !== undefined) { where.push("(scope_kind = 'agent' AND scope_key = ?)"); params.push(allow.agent) }
  if (allow.friend !== undefined) { where.push("(scope_kind = 'shared-friend' AND scope_key = ?)"); params.push(allow.friend) }
  if (allow.group !== undefined) { where.push("(scope_kind = 'shared-group' AND scope_key = ?)"); params.push(allow.group) }
  return { sql: where.length === 0 ? '0' : `(${where.join(' OR ')})`, params }
}

export class MemoryStore {
  private readonly db: DatabaseSync

  constructor(path: string) {
    mkdirSync(dirname(path), { recursive: true })
    this.db = new DatabaseSync(path)
    this.db.exec(`
      PRAGMA journal_mode = WAL;

      CREATE TABLE IF NOT EXISTS memories (
        id          INTEGER PRIMARY KEY AUTOINCREMENT,
        uid         TEXT NOT NULL UNIQUE,
        kind        TEXT NOT NULL,
        content     TEXT NOT NULL,
        scope_kind  TEXT NOT NULL,
        scope_key   TEXT,
        source_ch   TEXT NOT NULL,
        source_ref  TEXT NOT NULL DEFAULT '',
        weight      REAL NOT NULL DEFAULT 1.0,
        origin      TEXT NOT NULL DEFAULT 'auto',
        created_at  INTEGER NOT NULL,
        last_hit_at INTEGER,
        hit_count   INTEGER NOT NULL DEFAULT 0,
        embedding   BLOB
      );
      CREATE INDEX IF NOT EXISTS idx_mem_scope   ON memories(scope_kind, scope_key);
      CREATE INDEX IF NOT EXISTS idx_mem_created ON memories(created_at);

      CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
        uid UNINDEXED,
        content
      );
    `)
    // Migration: databases created before the origin column shipped lack it; the
    // CREATE TABLE IF NOT EXISTS above never alters an existing table.
    const cols = this.db.prepare('PRAGMA table_info(memories)').all() as Array<{ name: string }>
    if (!cols.some(c => c.name === 'origin')) {
      this.db.exec("ALTER TABLE memories ADD COLUMN origin TEXT NOT NULL DEFAULT 'auto'")
    }
  }

  /** 写入一条记忆；content 完全相同视为重复（P1 的简单去重，语义去重在提炼子智能体侧做）。 */
  add(input: NewMemory): MemoryRecord {
    const content = input.content.trim()
    if (content === '') throw new Error('memory content must not be empty')

    const existing = this.db.prepare(
      'SELECT * FROM memories WHERE scope_kind = ? AND scope_key IS ? AND content = ? LIMIT 1',
    ).get(scopeKind(input.scope), scopeKey(input.scope), content) as Record<string, unknown> | undefined
    if (existing !== undefined) return rowToRecord(existing)

    const uid = randomUUID()
    const now = Date.now()
    this.db.prepare(`
      INSERT INTO memories (uid, kind, content, scope_kind, scope_key, source_ch, source_ref, weight, origin, created_at)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `).run(
      uid,
      input.kind,
      content,
      scopeKind(input.scope),
      scopeKey(input.scope),
      input.sourceCh,
      input.sourceRef ?? '',
      input.weight ?? 1.0,
      input.origin ?? 'auto',
      now,
    )
    this.db.prepare('INSERT INTO memories_fts(uid, content) VALUES (?, ?)').run(uid, segment(content))
    const row = this.db.prepare('SELECT * FROM memories WHERE uid = ?').get(uid) as Record<string, unknown>
    return rowToRecord(row)
  }

  /** 按 scope 过滤 + 关键词 top-K 召回；命中后更新 lastHit/hitCount。 */
  retrieve(allow: AllowScopes, query: string, k = 8): MemoryRecord[] {
    const sc = scopeClause(allow)
    if (sc.sql === '0') return []
    const q = query.trim()
    const match = segment(q)
    if (match === '') {
      // 无关键词（空查询 / 纯标点）：FTS5 没有"匹配全部"的查询语法
      // （`MATCH '*'` 会抛 "unknown special query"），改为直接按 scope 列全量，
      // 权重优先、最新优先（无相关性排序）。
      const rows = this.db.prepare(
        `SELECT * FROM memories WHERE ${sc.sql} ORDER BY weight DESC, created_at DESC LIMIT ?`,
      ).all(...sc.params, k) as Array<Record<string, unknown>>
      const records = rows.map(rowToRecord)
      if (records.length > 0) this.touchHit(records.map(r => r.id))
      return records
    }
    const sql = `
      SELECT m.*
      FROM memories m
      JOIN memories_fts ON memories_fts.uid = m.uid
      WHERE memories_fts MATCH ? AND ${sc.sql}
      ORDER BY bm25(memories_fts), m.weight DESC, m.created_at DESC
      LIMIT ?
    `
    const rows = this.db.prepare(sql).all(match, ...sc.params, k) as Array<Record<string, unknown>>
    const records = rows.map(rowToRecord)
    if (records.length > 0) this.touchHit(records.map(r => r.id))
    return records
  }

  /** 列出某 scope 下的所有记忆（无关键词，全量给「记忆浏览」用，不进 prompt）。 */
  list(allow: AllowScopes): MemoryRecord[] {
    const sc = scopeClause(allow)
    if (sc.sql === '0') return []
    const rows = this.db.prepare(
      `SELECT * FROM memories WHERE ${sc.sql} ORDER BY created_at DESC`,
    ).all(...sc.params) as Array<Record<string, unknown>>
    return rows.map(rowToRecord)
  }

  remove(uid: string): boolean {
    const info = this.db.prepare('DELETE FROM memories WHERE uid = ?').run(uid)
    if ((info.changes as number) === 0) return false
    this.db.prepare("DELETE FROM memories_fts WHERE uid = ?").run(uid)
    return true
  }

  /** Edit one memory's content (and re-index its full-text row); an optional scope moves it between global / agent / group. */
  update(uid: string, content: string, scope?: MemoryScope): MemoryRecord | undefined {
    const next = content.trim()
    if (next === '') throw new Error('memory content must not be empty')
    const info = scope === undefined
      ? this.db.prepare('UPDATE memories SET content = ? WHERE uid = ?').run(next, uid)
      : this.db.prepare('UPDATE memories SET content = ?, scope_kind = ?, scope_key = ? WHERE uid = ?').run(next, scopeKind(scope), scopeKey(scope), uid)
    if ((info.changes as number) === 0) return undefined
    this.db.prepare('DELETE FROM memories_fts WHERE uid = ?').run(uid)
    this.db.prepare('INSERT INTO memories_fts(uid, content) VALUES (?, ?)').run(uid, segment(next))
    const row = this.db.prepare('SELECT * FROM memories WHERE uid = ?').get(uid) as Record<string, unknown>
    return rowToRecord(row)
  }

  /** One memory by uid (for editing). */
  get(uid: string): MemoryRecord | undefined {
    const row = this.db.prepare('SELECT * FROM memories WHERE uid = ?').get(uid) as Record<string, unknown> | undefined
    return row === undefined ? undefined : rowToRecord(row)
  }

  count(allow: AllowScopes): number {
    return this.list(allow).length
  }

  close(): void {
    this.db.close()
  }

  private touchHit(ids: number[]): void {
    const stmt = this.db.prepare('UPDATE memories SET last_hit_at = ?, hit_count = hit_count + 1 WHERE id = ?')
    const now = Date.now()
    for (const id of ids) stmt.run(now, id)
  }
}
