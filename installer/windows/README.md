# SoulMirror (DSH edition) — Windows installer

Produces `dist/SoulMirror-DSH-Setup-<version>.exe`: a per-user Windows
installer that ships DeepSeek Harness (dsh) with the `soulnet-dsh` plugin
pre-installed, launched from a Start-menu / desktop shortcut named
**SoulMirror**. No admin rights, no code signing, no autostart.

## Build

```sh
bash scripts/build-installer-win.sh
```

Requirements: Windows + Git Bash, Node.js >= 22.19, pnpm, Go, Inno Setup 6
(`winget install -e --id JRSoftware.InnoSetup`), network access (nodejs.org
and the npm registry). The portable Node.js zip is cached in
`.installer-cache/`. Overrides: `NODE_VERSION`, `DSH_VERSION`,
`PNPM_VERSION`, `ISCC` (path to ISCC.exe), `SKIP_ISCC=1` (stage only).

The installer version is the `soulnet-dsh` version
(`dsh/packages/dsh/package.json`); the peer binary is built from this
repository's Go source with that same version baked in.

## What the installer lays down

```
%LOCALAPPDATA%\Programs\SoulMirror-DSH\
├── node\            portable Node.js (official nodejs.org win-x64 zip)
├── app\             @deepseek-ai/dsh CLI + pnpm (plain npm install)
├── home-template\   a pre-installed dsh web profile (see below)
├── bin\soulmirror-dsh.cmd   the launcher the shortcuts point to
└── soulmirror.ico
```

The directory is deliberately **not** `%LOCALAPPDATA%\Programs\SoulMirror` —
that belongs to the main SoulMirror desktop product. Separate `AppId`,
separate directory; the two never overwrite each other.

## How the plugin is loaded (why the template looks like it does)

dsh keeps per-profile plugin state under `$DSH_HOME/profiles/<name>/`:

- `package.json` — the profile manifest: out-of-tree plugin `dependencies`
  plus `dsh.profile.bundles`, the ordered patch-layer list. Our template
  ships `["@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app", "soulnet-dsh"]`
  — exactly what `dsh plugin --profile web add soulnet-dsh` would write.
- `node_modules/` — pnpm-managed for out-of-tree plugins. Boot never runs
  pnpm (only `dsh plugin` does), so the template seeds it directly from the
  npm tarballs packed out of this repository: `soulnet-dsh`,
  `soulnet-peer-windows-x64` (the Go peer binary; the plugin resolves it
  "next to the plugin", spawns it over stdio JSON-RPC), and
  `soulnet-dsh-sidebar` (SoulMirror branding; not a `dependencies` entry —
  it is resolved through soulnet-dsh's client-inject list, so shipping the
  directory is enough).
- `cordis.patch.yml` — the user's own override layer; shipped empty (`[]`),
  so the plugin defaults apply: relay `https://relay.startupworld.cn`, data
  in `~/.soulnet`, onboarding asks for a display name.
- In-box bundles (`@deepseek-ai/dsh-*`) are **not** copied into the profile:
  on boot dsh maintains `$DSH_HOME/profiles/node_modules` as junctions into
  the dsh installation (`app\node_modules` here) and resolves them there.

## First run and upgrades (launcher logic)

`bin\soulmirror-dsh.cmd` sets `DSH_HOME=%USERPROFILE%\.dsh-soulmirror`
(dedicated — an existing `~\.dsh` is never touched), copies `home-template\`
there when the web profile is missing (offline, no pnpm), and boots
`dsh web` with the bundled Node — the browser opens on the local UI
(default port 3080). On upgrade, a template marker
(`.soulmirror-template-version`) tells the launcher to refresh only the
three shipped packages inside the profile, never the user's own plugins or
settings. Arguments are forwarded: `soulmirror-dsh.cmd --no-open --port 3210`.

Uninstall removes only the program directory. User data stays:
`~\.dsh-soulmirror` (dsh sessions/settings) and `~\.soulnet` (identity,
friends, conversations).
