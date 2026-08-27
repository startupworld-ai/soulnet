// SoulMirror desktop shell: starts the dsh web server (hidden, no console) and
// shows the local UI in a native window — no browser, no cmd popup.
//
// Layout (relative to this file's __dirname = <root>/desktop/app):
//   <root>/node           portable Node.js (runs `dsh web`)
//   <root>/app            the @deepseek-ai/dsh CLI
//   <root>/home-template  the pre-installed dsh web profile
const { app, BrowserWindow, Menu, shell } = require('electron')
const { spawn } = require('node:child_process')
const path = require('node:path')
const fs = require('node:fs')
const os = require('node:os')
const http = require('node:http')

const PORT = Number(process.env.SOULMIRROR_PORT || 3080)
function resolveRoot() {
  if (process.env.SOULMIRROR_ROOT) return process.env.SOULMIRROR_ROOT
  // The install root is the directory that holds home-template (+ node/, app/).
  // Windows (raw electron dist): __dirname = <root>/desktop/app -> 2 levels up.
  // macOS (packaged .app):       __dirname = <app>/Contents/Resources/app -> 1 level up.
  for (const candidate of [path.resolve(__dirname, '..', '..'), path.resolve(__dirname, '..')]) {
    if (fs.existsSync(path.join(candidate, 'home-template'))) return candidate
  }
  return path.resolve(__dirname, '..', '..')
}
const ROOT = resolveRoot()
const SHIPPED_PACKAGES = ['soulnet-dsh', 'soulnet-peer-windows-x64', 'soulnet-peer-darwin-x64', 'soulnet-peer-darwin-arm64', 'soulnet-dsh-sidebar']

function nodeBin() {
  if (process.platform !== 'win32') {
    const arch = process.arch === 'arm64' ? 'arm64' : 'x64'
    return path.join(ROOT, `node-${arch}`, 'bin', 'node')
  }
  return path.join(ROOT, 'node', 'node.exe')
}

function seedHome(dshHome) {
  const template = path.join(ROOT, 'home-template')
  const profile = path.join(dshHome, 'profiles', 'web')
  const mark = '.soulmirror-template-version'
  const tplMark = path.join(template, 'profiles', 'web', mark)
  const profileMark = path.join(profile, mark)
  try {
    if (!fs.existsSync(path.join(profile, 'package.json'))) {
      // First run: seed the whole profile (offline, no pnpm).
      fs.mkdirSync(dshHome, { recursive: true })
      fs.cpSync(template, dshHome, { recursive: true })
      return
    }
    // Upgrade: refresh ONLY the packages we ship when the template changed.
    let tpl = ''
    let cur = ''
    try { tpl = fs.readFileSync(tplMark, 'utf8') } catch {}
    try { cur = fs.readFileSync(profileMark, 'utf8') } catch {}
    if (tpl !== '' && tpl !== cur) {
      for (const p of SHIPPED_PACKAGES) {
        const src = path.join(template, 'profiles', 'web', 'node_modules', p)
        const dst = path.join(profile, 'node_modules', p)
        if (fs.existsSync(src)) {
          fs.rmSync(dst, { recursive: true, force: true })
          fs.cpSync(src, dst, { recursive: true })
        }
      }
      fs.copyFileSync(tplMark, profileMark)
    }
  } catch (e) {
    console.error('seed profile failed:', e)
  }
}

function startServer(dshHome) {
  const bin = path.join(ROOT, 'app', 'node_modules', '@deepseek-ai', 'dsh', 'lib', 'bin.js')
  const child = spawn(nodeBin(), [bin, 'web', '--no-open', '--port', String(PORT)], {
    env: { ...process.env, DSH_HOME: dshHome },
    stdio: 'ignore',
    windowsHide: true,
  })
  child.on('error', (e) => { console.error('failed to start dsh web:', e) })
  child.on('exit', (code) => { if (code !== 0 && code !== null) console.error('dsh web exited:', code) })
  return child
}

function waitForServer(url, timeoutMs) {
  return new Promise((resolve) => {
    const deadline = Date.now() + timeoutMs
    const poll = () => {
      const req = http.get(url, (res) => { res.resume(); resolve(true) })
      req.on('error', () => { schedule() })
      req.setTimeout(800, () => { req.destroy(); schedule() })
      function schedule() {
        if (Date.now() > deadline) resolve(false)
        else setTimeout(poll, 300)
      }
    }
    poll()
  })
}

let server = null

app.whenReady().then(async () => {
  const dshHome = process.env.DSH_HOME || path.join(os.homedir(), '.dsh-soulmirror')
  seedHome(dshHome)
  server = startServer(dshHome)
  const ok = await waitForServer(`http://127.0.0.1:${PORT}/soulmirror/api/state`, 90000)
  if (!ok) {
    const err = new BrowserWindow({ width: 520, height: 260, title: 'SoulMirror', autoHideMenuBar: true })
    err.loadURL('data:text/html;charset=utf-8,' + encodeURIComponent(
      '<body style="font-family:system-ui;padding:32px;background:#1e1f22;color:#e6e6e6">' +
      '<h3>无法启动 SoulMirror</h3><p>dsh 服务未能启动，请关闭后重试。</p></body>'))
    return
  }
  const win = new BrowserWindow({
    width: 1280,
    height: 840,
    minWidth: 960,
    minHeight: 600,
    title: 'SoulMirror',
    autoHideMenuBar: true,
    backgroundColor: '#1e1f22',
    ...(process.platform === 'win32' ? { icon: path.join(ROOT, 'soulmirror.ico') } : {}),
  })
  Menu.setApplicationMenu(null)
  win.loadURL(`http://127.0.0.1:${PORT}`)
  win.webContents.setWindowOpenHandler(({ url }) => {
    if (!url.startsWith(`http://127.0.0.1:${PORT}`)) {
      shell.openExternal(url)
      return { action: 'deny' }
    }
    return { action: 'allow' }
  })
  win.on('closed', () => { app.quit() })
})

app.on('window-all-closed', () => { app.quit() })
app.on('will-quit', () => { if (server) { try { server.kill() } catch {} } })
