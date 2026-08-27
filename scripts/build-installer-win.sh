#!/usr/bin/env bash
# scripts/build-installer-win.sh -- build the SoulMirror (DSH edition) Windows installer.
#
# One command produces dist/SoulMirror-DSH-Setup-<version>.exe:
#   1. pnpm install + build the soulnet-dsh plugin (and the sidebar brand
#      package) from this repository;
#   2. go-build the soulnet light peer for windows-x64 into its platform
#      npm package, then pack the tarballs (scripts already in dsh/scripts/);
#   3. stage a self-contained tree:
#        (stage defaults to /tmp/smdsh-stage; see below)
#        node/           portable Node.js (official nodejs.org zip, cached)
#        app/            @deepseek-ai/dsh CLI + pnpm, npm-installed
#        home-template/  a pre-installed dsh web profile: package.json with
#                        soulnet-dsh as dependency + bundle, node_modules
#                        seeded from OUR tarballs (offline first run)
#        bin/            launcher (installer/windows/launcher.cmd)
#   4. compile installer/windows/soulmirror-dsh.iss with Inno Setup 6.
#
# Windows only (ISCC is a Windows program); run from Git Bash / MSYS.
# Requirements: pnpm, Go, Inno Setup 6, network (nodejs.org + npm registry).
#
# Environment overrides:
#   NODE_VERSION  portable Node.js version   (default: 22.23.1)
#   DSH_VERSION   @deepseek-ai/dsh version   (default: the dsh-* version the
#                                             plugin is developed against)
#   PNPM_VERSION  pnpm bundled into app/     (default: dsh/package.json packageManager)
#   ISCC          path to ISCC.exe           (default: probed)
#   SKIP_ISCC=1   stage only, no installer (for CI smoke on machines without Inno)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Windows node.exe does not understand MSYS /d/... paths; feed it mixed form.
wpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s
' "$1"; fi; }
DSH_DIR="$ROOT/dsh"
# The stage must live at a SHORT path: the dsh closure contains files ~200
# chars deep (node_modules/@earendil-works/pi-ai/node_modules/@mistralai/...),
# and ISCC hits Windows MAX_PATH when the prefix is a deep repo checkout.
# /tmp maps to %LOCALAPPDATA%\Temp in Git Bash. Override with SMDSH_STAGE.
STAGE="${SMDSH_STAGE:-/tmp/smdsh-stage}"
CACHE="$ROOT/.installer-cache"
DIST="$ROOT/dist"
ISS="$ROOT/installer/windows/soulmirror-dsh.iss"

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) ;;
  *) if [[ "${SKIP_ISCC:-0}" != "1" ]]; then
       echo "error: ISCC runs on Windows only; set SKIP_ISCC=1 to stage without packing" >&2
       exit 1
     fi ;;
esac

DSH_DIR_W="$(wpath "$DSH_DIR")"
VERSION="$(node -p "require('$DSH_DIR_W/packages/dsh/package.json').version")"
DSH_VERSION="${DSH_VERSION:-$(node -p "require('$DSH_DIR_W/packages/dsh/package.json').devDependencies['@deepseek-ai/dsh-agent']")}"
PNPM_VERSION="${PNPM_VERSION:-$(node -p "require('$DSH_DIR_W/package.json').packageManager.split('@')[1]")}"
NODE_VERSION="${NODE_VERSION:-22.23.1}"
# VersionInfoVersion wants plain a.b.c.d: strip any prerelease tag, pad with .0.
BASE_VERSION="${VERSION%%-*}"
FILE_VERSION="$BASE_VERSION.0"

echo "== SoulMirror (DSH edition) installer =="
echo "   plugin soulnet-dsh $VERSION | dsh $DSH_VERSION | node $NODE_VERSION | pnpm $PNPM_VERSION"

# ---------------------------------------------------------------- [1] plugin
echo "-- [1/6] pnpm install + build (soulnet-dsh, soulnet-dsh-sidebar)"
( cd "$DSH_DIR" && pnpm install --frozen-lockfile )
( cd "$DSH_DIR" && pnpm --filter soulnet-dsh --filter soulnet-dsh-sidebar run build )

# ------------------------------------------------------------------ [2] peer
echo "-- [2/6] go build soulnet peer (windows-x64) + pack tarballs"
# Same flags as dsh/scripts/build-peer-packages.mjs, but built directly: that
# script gates on platform-package version == plugin version, which is a
# release invariant, not an installer one (npm currently has peer 0.1.0 next
# to plugin 0.1.1 by design -- workspace:* rewrites to the real version).
( cd "$ROOT" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0     go build -trimpath -ldflags "-s -w -X main.Version=$VERSION"     -o "$DSH_DIR/packages/soulnet-win32-x64/bin/soulnet.exe" ./cmd/soulnet )
( cd "$ROOT" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0     go build -trimpath -ldflags "-s -w -X main.Version=$VERSION"     -o "$DSH_DIR/packages/soulnet-paygate-win32-x64/bin/paygate.exe" ./payment/cmd/paygate )
rm -rf "$STAGE"
TARBALLS="$ROOT/.installer-stage/tarballs"
rm -rf "$TARBALLS"
mkdir -p "$TARBALLS"
for pkg in soulnet-win32-x64 soulnet-paygate-win32-x64 dsh sidebar; do
  ( cd "$DSH_DIR/packages/$pkg" && pnpm pack --pack-destination "$TARBALLS" >/dev/null )
done

PLUGIN_TGZ="$(ls "$TARBALLS"/soulnet-dsh-[0-9]*.tgz)"
PEER_TGZ="$(ls "$TARBALLS"/soulnet-peer-windows-x64-*.tgz)"
PAYGATE_TGZ="$(ls "$TARBALLS"/soulnet-paygate-windows-x64-*.tgz)"
SIDEBAR_TGZ="$(ls "$TARBALLS"/soulnet-dsh-sidebar-*.tgz)"

# ------------------------------------------------------- [3] portable node
echo "-- [3/6] portable Node.js v$NODE_VERSION"
mkdir -p "$CACHE"
NODE_ZIP="$CACHE/node-v$NODE_VERSION-win-x64.zip"
if [[ ! -f "$NODE_ZIP" ]]; then
  curl -fL --retry 3 -o "$NODE_ZIP.part" \
    "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-win-x64.zip"
  mv "$NODE_ZIP.part" "$NODE_ZIP"
fi
# Windows' bundled bsdtar understands zip; GNU tar in Git Bash does not.
mkdir -p "$STAGE/unzip"
MSYS2_ARG_CONV_EXCL='*' "$WINDIR/System32/tar.exe" -xf "$(cygpath -w "$NODE_ZIP")" -C "$(cygpath -w "$STAGE/unzip")"
mv "$STAGE/unzip/node-v$NODE_VERSION-win-x64" "$STAGE/node"
rmdir "$STAGE/unzip"
STAGED_NODE="$STAGE/node/node.exe"

# --------------------------------------------------------------- [4] dsh CLI
# pnpm with nodeLinker=hoisted: an npm-like flat node_modules of real files
# (relocatable, no virtual store), and pnpm resolves the large dsh closure in
# seconds where npm's resolver chokes on the rc-tagged peer ranges.
echo "-- [4/6] pnpm install @deepseek-ai/dsh@$DSH_VERSION + pnpm@$PNPM_VERSION into app/"
mkdir -p "$STAGE/app"
cat > "$STAGE/app/package.json" <<EOF
{
  "name": "soulmirror-dsh-app",
  "private": true,
  "dependencies": {
    "@deepseek-ai/dsh": "$DSH_VERSION",
    "pnpm": "$PNPM_VERSION"
  }
}
EOF
cat > "$STAGE/app/pnpm-workspace.yaml" <<'EOF'
packages:
  - .

nodeLinker: hoisted
# npm-like semantics: the dsh app expects its peer dependencies present
# (dsh-app-boot peers on @deepseek-ai/cordis-plugin-group etc. -- the
# documented install path is npm/npx, which auto-installs peers).
autoInstallPeers: true
# Run these dependencies' install scripts on the BUILD machine so their
# native artifacts (node-pty conpty, koffi prebuilds, ...) ship ready-made;
# pnpm otherwise blocks them (ERR_PNPM_IGNORED_BUILDS).
allowBuilds:
  '@deepseek-ai/dsh-subprocess-local': true
  '@google/genai': true
  koffi: true
  node-pty: true
  protobufjs: true
EOF
( cd "$STAGE/app" && pnpm install )
[[ -f "$STAGE/app/node_modules/@deepseek-ai/dsh/lib/bin.js" ]] || {
  echo "error: dsh CLI missing after pnpm install" >&2; exit 1; }

# --------------------------------------------------------- [5] home template
echo "-- [5/6] dsh home template (pre-installed web profile)"
WEB="$STAGE/home-template/profiles/web"
mkdir -p "$WEB/node_modules"
"$STAGED_NODE" -e '
  const fs = require("fs");
  const [web, version] = process.argv.slice(1);
  fs.writeFileSync(web + "/package.json", JSON.stringify({
    name: "dsh-profile-web",
    private: true,
    dependencies: { "soulnet-dsh": version },
    dsh: { profile: { bundles: [
      "@deepseek-ai/dsh-base",
      "@deepseek-ai/dsh-web-app",
      "soulnet-dsh",
      "soulnet-dsh-sidebar",
    ] } },
  }, undefined, 2) + "\n");
' "$(wpath "$WEB")" "$VERSION"
cat > "$WEB/cordis.patch.yml" <<'EOF'
# Your patch layer for this dsh profile, applied after every bundle layer:
# a top-level YAML array of loader patch entries (id-targeted config
# overrides, disables, and insert lists; `!!js` expressions allowed).
[]
EOF
cat > "$WEB/pnpm-workspace.yaml" <<'EOF'
packages:
  - .

nodeLinker: hoisted
autoInstallPeers: false
EOF
# node_modules seeded straight from our tarballs (exactly the files npm would
# lay down; boot never runs pnpm, so no lockfile is needed).
# soulnet-dsh-sidebar (SoulMirror brand + left column) is a bundle row but NOT
# a package.json dependency: it is unpublished, and bundle names resolve by
# plain Node resolution from the profile dir, so shipping the directory is
# enough (`dsh plugin` reconciles only dependency-listed names, so it will
# neither fetch nor drop it).
for tgz in "$PLUGIN_TGZ" "$PEER_TGZ" "$PAYGATE_TGZ" "$SIDEBAR_TGZ"; do
  rm -rf "$STAGE/pkg"
  mkdir -p "$STAGE/pkg"
  tar -xzf "$tgz" -C "$STAGE/pkg"
  name="$("$STAGED_NODE" -p "require('$(wpath "$STAGE")/pkg/package/package.json').name")"
  mv "$STAGE/pkg/package" "$WEB/node_modules/$name"
done
rm -rf "$STAGE/pkg"
[[ -f "$WEB/node_modules/soulnet-peer-windows-x64/bin/soulnet.exe" ]] || {
  echo "error: peer binary missing from template" >&2; exit 1; }
[[ -f "$WEB/node_modules/soulnet-paygate-windows-x64/bin/paygate.exe" ]] || {
  echo "error: paygate binary missing from template" >&2; exit 1; }
# Template marker: the launcher refreshes the shipped packages when it changes.
PLUGIN_SHA="$(sha256sum "$PLUGIN_TGZ" | cut -c1-16)"
printf '%s %s\n' "$VERSION" "$PLUGIN_SHA" > "$WEB/.soulmirror-template-version"

mkdir -p "$STAGE/bin"
cp "$ROOT/installer/windows/launcher.cmd" "$STAGE/bin/soulmirror-dsh.cmd"

# ----------------------------------------------------- [5b] Electron desktop shell
echo "-- [5b/7] Electron desktop shell"
ELECTRON_VERSION="${ELECTRON_VERSION:-33.2.0}"
mkdir -p "$STAGE/desktop-pkg"
cat > "$STAGE/desktop-pkg/package.json" <<EOF
{ "name": "smdsh-desktop-deps", "private": true, "dependencies": { "electron": "$ELECTRON_VERSION" } }
EOF
( cd "$STAGE/desktop-pkg" && ELECTRON_MIRROR=https://npmmirror.com/mirrors/electron/ npm install --no-audit --no-fund )
mkdir -p "$STAGE/desktop"
cp -R "$STAGE/desktop-pkg/node_modules/electron/dist/." "$STAGE/desktop/"
mkdir -p "$STAGE/desktop/app"
cp "$ROOT/desktop/app/main.js" "$STAGE/desktop/app/main.js"
cp "$ROOT/desktop/app/package.json" "$STAGE/desktop/app/package.json"
[[ -f "$STAGE/desktop/electron.exe" ]] || { echo "error: electron.exe missing" >&2; exit 1; }
rm -rf "$STAGE/desktop-pkg"

# ------------------------------------------------------------------ [6] ISCC
if [[ "${SKIP_ISCC:-0}" == "1" ]]; then
  echo "-- [6/6] SKIP_ISCC=1: staged at $STAGE, no installer compiled"
  exit 0
fi
echo "-- [6/6] Inno Setup"
if [[ -z "${ISCC:-}" ]]; then
  for candidate in \
    "$LOCALAPPDATA/Programs/Inno Setup 6/ISCC.exe" \
    "/c/Program Files (x86)/Inno Setup 6/ISCC.exe" \
    "/c/Program Files/Inno Setup 6/ISCC.exe"; do
    [[ -f "$candidate" ]] && ISCC="$candidate" && break
  done
fi
[[ -n "${ISCC:-}" && -f "$ISCC" ]] || {
  echo "error: ISCC.exe not found; install Inno Setup 6 (winget install -e --id JRSoftware.InnoSetup) or set ISCC=<path>" >&2
  exit 1; }
mkdir -p "$DIST"
# MSYS would otherwise rewrite the /D... switches as filesystem paths, and
# ISCC then sees them as extra script filenames.
MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' "$ISCC" \
  "/DAppVersion=$VERSION" \
  "/DFileVersion=$FILE_VERSION" \
  "/DSourceDir=$(cygpath -w "$STAGE")" \
  "/DOutputDir=$(cygpath -w "$DIST")" \
  "$(cygpath -w "$ISS")"

OUT="$DIST/SoulMirror-DSH-Setup-$VERSION.exe"
[[ -f "$OUT" ]] || { echo "error: expected $OUT" >&2; exit 1; }
echo "== done: $OUT ($(du -h "$OUT" | cut -f1))"
