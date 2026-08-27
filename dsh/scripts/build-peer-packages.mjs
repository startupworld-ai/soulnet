#!/usr/bin/env node
/**
 * Cross-compile the `soulnet` light peer into the five platform npm packages
 * (dsh/packages/soulnet-<os>-<arch>/bin/soulnet[.exe]) and, optionally, the
 * GitHub Release assets (soulnet + soulnet-relay per platform + SHA256SUMS).
 *
 *   node dsh/scripts/build-peer-packages.mjs                 # all five packages
 *   node dsh/scripts/build-peer-packages.mjs --current       # only the host platform (dev loop)
 *   node dsh/scripts/build-peer-packages.mjs --targets win32-x64,linux-arm64
 *   node dsh/scripts/build-peer-packages.mjs --assets dist/release   # + release assets
 *
 * Needs a Go toolchain (CGO_ENABLED=0, -trimpath -ldflags "-s -w"). The
 * version baked into the binary (`soulnet --version`) is the plugin's
 * package.json version. Binaries are git-ignored (root .gitignore: bin/).
 */
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const dshDir = resolve(here, '..')
const repoRoot = resolve(dshDir, '..')

/** os/arch as npm spells them -> as Go spells them. */
const TARGETS = [
  { id: 'win32-x64', goos: 'windows', goarch: 'amd64', exe: '.exe' },
  { id: 'darwin-arm64', goos: 'darwin', goarch: 'arm64', exe: '' },
  { id: 'darwin-x64', goos: 'darwin', goarch: 'amd64', exe: '' },
  { id: 'linux-x64', goos: 'linux', goarch: 'amd64', exe: '' },
  { id: 'linux-arm64', goos: 'linux', goarch: 'arm64', exe: '' },
]

const args = process.argv.slice(2)
const flag = (name) => {
  const i = args.indexOf(name)
  return i === -1 ? undefined : (args[i + 1] ?? '')
}
const currentId = `${process.platform}-${process.arch}`
let wanted = TARGETS
if (args.includes('--current')) wanted = TARGETS.filter(t => t.id === currentId)
const list = flag('--targets')
if (list !== undefined) {
  const ids = list.split(',').map(s => s.trim()).filter(Boolean)
  wanted = TARGETS.filter(t => ids.includes(t.id))
  const unknown = ids.filter(id => !TARGETS.some(t => t.id === id))
  if (unknown.length > 0) {
    console.error(`unknown target(s): ${unknown.join(', ')}; known: ${TARGETS.map(t => t.id).join(', ')}`)
    process.exit(2)
  }
}
if (wanted.length === 0) {
  console.error(`no target matches (host is ${currentId})`)
  process.exit(2)
}
const assetsDir = flag('--assets')
const version = JSON.parse(readFileSync(join(dshDir, 'packages', 'dsh', 'package.json'), 'utf8')).version

function goBuild(target, pkg, out) {
  mkdirSync(dirname(out), { recursive: true })
  const ldflags = `-s -w -X main.Version=${version}`
  const result = spawnSync('go', ['build', '-trimpath', '-ldflags', ldflags, '-o', out, pkg], {
    cwd: repoRoot,
    stdio: 'inherit',
    env: { ...process.env, GOOS: target.goos, GOARCH: target.goarch, CGO_ENABLED: '0' },
  })
  if (result.error) {
    console.error(`go build failed to start: ${result.error.message} (is Go installed?)`)
    process.exit(1)
  }
  if (result.status !== 0) {
    console.error(`go build ${pkg} for ${target.id} failed (exit ${result.status})`)
    process.exit(result.status ?? 1)
  }
  console.log(`built ${pkg} ${target.id} -> ${out}`)
}

for (const target of wanted) {
  const pkgDir = join(dshDir, 'packages', `soulnet-${target.id}`)
  if (!existsSync(join(pkgDir, 'package.json'))) {
    console.error(`missing platform package ${pkgDir}`)
    process.exit(1)
  }
  const pkgVersion = JSON.parse(readFileSync(join(pkgDir, 'package.json'), 'utf8')).version
  if (pkgVersion !== version) {
    console.error(`version mismatch: ${pkgDir} is ${pkgVersion}, plugin is ${version}`)
    process.exit(1)
  }
  goBuild(target, './cmd/soulnet', join(pkgDir, 'bin', `soulnet${target.exe}`))

  // The local payment gateway (paygate) rides the same platform packages:
  // <pkg>/bin/paygate[.exe] is resolved by the plugin when CDP is configured.
  const paygatePkgDir = join(dshDir, 'packages', `soulnet-paygate-${target.id}`)
  if (existsSync(join(paygatePkgDir, 'package.json'))) {
    const paygateVersion = JSON.parse(readFileSync(join(paygatePkgDir, 'package.json'), 'utf8')).version
    if (paygateVersion !== version) {
      console.error(`version mismatch: ${paygatePkgDir} is ${paygateVersion}, plugin is ${version}`)
      process.exit(1)
    }
    goBuild(target, './payment/cmd/paygate', join(paygatePkgDir, 'bin', `paygate${target.exe}`))
  } else {
    console.warn(`skipping paygate for ${target.id}: no ${paygatePkgDir}`)
  }
  if (assetsDir !== undefined && assetsDir !== '') {
    const dir = resolve(process.cwd(), assetsDir)
    goBuild(target, './cmd/soulnet', join(dir, `soulnet-${target.id}${target.exe}`))
    goBuild(target, './cmd/soulnet-relay', join(dir, `soulnet-relay-${target.id}${target.exe}`))
    goBuild(target, './payment/cmd/paygate', join(dir, `soulnet-paygate-${target.id}${target.exe}`))
  }
}

if (assetsDir !== undefined && assetsDir !== '') {
  const dir = resolve(process.cwd(), assetsDir)
  const lines = []
  for (const name of readdirSync(dir).sort()) {
    if (name === 'SHA256SUMS.txt') continue
    const digest = createHash('sha256').update(readFileSync(join(dir, name))).digest('hex')
    lines.push(`${digest}  ${name}`)
  }
  writeFileSync(join(dir, 'SHA256SUMS.txt'), `${lines.join('\n')}\n`)
  console.log(`wrote ${join(dir, 'SHA256SUMS.txt')} (${lines.length} files)`)
}
