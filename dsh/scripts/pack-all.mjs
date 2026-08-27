#!/usr/bin/env node
/**
 * Pack every publishable package of the workspace into dsh/dist/ as npm
 * tarballs (`pnpm pack`, which rewrites `workspace:*` ranges to real versions
 * exactly like `pnpm publish` does): the five platform packages first, then
 * the plugin. Used by the local fresh-user install test (README "Install" ->
 * "Verify a release locally") and as a dry run of the release workflow.
 *
 *   pnpm run pack:all                    # all six; fails when a binary is missing
 *   pnpm run pack:all -- --allow-missing # pack the platform packages that have a binary, skip the rest
 *   pnpm run pack:all -- --out some/dir  # another destination
 */
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, readdirSync, rmSync, statSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const dshDir = resolve(here, '..')
const args = process.argv.slice(2)
const allowMissing = args.includes('--allow-missing')
const outIndex = args.indexOf('--out')
const out = resolve(process.cwd(), outIndex === -1 ? join(dshDir, 'dist') : (args[outIndex + 1] ?? 'dist'))

mkdirSync(out, { recursive: true })
for (const name of readdirSync(out)) if (name.endsWith('.tgz')) rmSync(join(out, name))

const platformDirs = readdirSync(join(dshDir, 'packages'))
  .filter(name => name.startsWith('soulnet-'))
  .map(name => join(dshDir, 'packages', name))
  .sort()
const packages = []
for (const dir of platformDirs) {
  const exe = dir.includes('win32') ? '.exe' : ''
  // A platform package ships either the peer binary (bin/soulnet[.exe]) or the
  // payment gateway (bin/paygate[.exe]); check for whichever this package is.
  const binName = dir.includes('paygate') ? `paygate${exe}` : `soulnet${exe}`
  const bin = join(dir, 'bin', binName)
  if (!existsSync(bin)) {
    const msg = `${dir}: no ${bin} -- run scripts/build-peer-packages.mjs first`
    if (!allowMissing) {
      console.error(msg)
      process.exit(1)
    }
    console.warn(`skip: ${msg}`)
    continue
  }
  packages.push(dir)
}
packages.push(join(dshDir, 'packages', 'dsh'))

for (const dir of packages) {
  const result = spawnSync('pnpm', ['pack', '--pack-destination', out], { cwd: dir, stdio: 'inherit', shell: process.platform === 'win32' })
  if (result.status !== 0) {
    console.error(`pnpm pack failed in ${dir}`)
    process.exit(result.status ?? 1)
  }
}
console.log(`\ntarballs in ${out}:`)
for (const name of readdirSync(out).filter(n => n.endsWith('.tgz')).sort()) {
  console.log(`  ${name}  (${(statSync(join(out, name)).size / 1024).toFixed(0)} KiB)`)
}
