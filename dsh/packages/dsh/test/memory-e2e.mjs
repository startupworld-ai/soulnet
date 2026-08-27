/**
 * Playwright end-to-end test for the SoulMirror memory page (requirement 6):
 * the full CRUD round-trip through the real stack — browser → `/soulmirror/api/`
 * HTTP API → sessions plugin → SQLite memory store — against a running dsh web.
 *
 * What it checks (on the alter's memory page, scope = global):
 *   1. the SoulMirror page opens from the sidebar entry;
 *   2. the memory tab renders the memory pane;
 *   3. adding a memory lists it back with origin=manual + scope=global;
 *   4. editing it persists the new content;
 *   5. deleting it empties it again;
 *   6. no uncaught page/console errors happened during the run.
 *
 * Run (any playwright install works; point PLAYWRIGHT at one when it is not a
 * dependency of this package):
 *
 *   SOULMIRROR_URL=http://127.0.0.1:3099 \
 *   PLAYWRIGHT=D:/path/to/node_modules/playwright \
 *   node test/memory-e2e.mjs
 *
 * Defaults: SOULMIRROR_URL=http://127.0.0.1:3099, PLAYWRIGHT=playwright.
 */
import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
const { chromium } = require(process.env.PLAYWRIGHT ?? 'playwright')

const BASE = process.env.SOULMIRROR_URL ?? 'http://127.0.0.1:3099'
const VIEWPORT = { width: 1440, height: 900 }

let failed = false
function fail(msg) {
  failed = true
  console.error(`\nFAIL: ${msg}`)
}

async function main() {
  const browser = await chromium.launch({ headless: true })
  const page = await browser.newPage({ viewport: VIEWPORT })
  const pageErrors = []
  page.on('pageerror', (e) => { pageErrors.push(`pageerror: ${e.message}`) })
  page.on('console', (m) => { if (m.type() === 'error') pageErrors.push(`console.error: ${m.text()}`) })

  try {
    await page.goto(BASE, { waitUntil: 'domcontentloaded', timeout: 30000 })
    console.log(`1. loaded ${BASE}`)

    // The entry is either in the primary nav (SoulMirror sidebar installed) or the
    // sidebar footer (stock dsh); exactly one renders.
    const entry = page.locator('[data-soulmirror-nav], [data-soulmirror-footer]').first()
    await entry.waitFor({ timeout: 30000 })
    await entry.click()
    console.log('2. opened the SoulMirror page')

    await page.waitForSelector('[data-soulmirror-page-chat="alter"]', { timeout: 30000 })

    await page.waitForSelector('[data-soulmirror-pane-tab="memory"]', { timeout: 10000 })
    await page.click('[data-soulmirror-pane-tab="memory"]')
    await page.waitForSelector('[data-soulmirror-memory-pane]', { timeout: 10000 })
    console.log('3. memory pane open')

    // ——— add ———
    const content = `e2e-记忆-篮球-${Date.now()}`
    await page.fill('[data-soulmirror-memory-draft]', content)
    await page.click('[data-soulmirror-memory-add]')
    const card = page.locator('[data-soulmirror-memory]').filter({ hasText: content })
    await card.waitFor({ timeout: 10000 })
    const uid = await card.getAttribute('data-soulmirror-memory')
    const origin = await card.locator('[data-soulmirror-memory-origin]').getAttribute('data-soulmirror-memory-origin')
    const scope = await card.locator('[data-soulmirror-memory-scope]').getAttribute('data-soulmirror-memory-scope')
    if (origin !== 'manual') throw new Error(`expected origin=manual, got ${origin}`)
    if (scope !== 'global') throw new Error(`expected scope=global, got ${scope}`)
    console.log(`4. add OK -> uid=${uid} origin=${origin} scope=${scope}`)

    // ——— edit ———
    const cardByUid = page.locator(`[data-soulmirror-memory="${uid}"]`)
    await cardByUid.locator('[data-soulmirror-memory-edit]').click()
    await cardByUid.locator('textarea').fill(`${content}-改`)
    await cardByUid.locator('[data-soulmirror-memory-save]').click()
    await cardByUid.filter({ hasText: `${content}-改` }).waitFor({ timeout: 10000 })
    console.log('5. edit OK')

    // ——— delete ———
    await cardByUid.locator('[data-soulmirror-memory-delete]').click()
    await cardByUid.waitFor({ state: 'detached', timeout: 10000 })
    console.log('6. delete OK')

    if (pageErrors.length > 0) {
      console.log('\nPAGE ERRORS:')
      for (const e of pageErrors) console.log(`  ${e}`)
      throw new Error(`${pageErrors.length} page error(s) detected`)
    }

    console.log(`\nPASS: memory CRUD round-trip (add → list → edit → delete) over ${BASE}`)
  } catch (e) {
    fail(e && e.message ? e.message : String(e))
    if (pageErrors.length > 0) {
      console.log('\nPAGE ERRORS:')
      for (const err of pageErrors) console.log(`  ${err}`)
    }
  } finally {
    await browser.close()
  }
}

main().then(() => {
  if (failed) process.exit(1)
})
