#!/usr/bin/env bash
# scripts/build-installer-mac.sh -- build the SoulMirror (DSH edition) macOS installer (.dmg).
#
# One command produces dist/SoulMirror-DSH-<version>.dmg containing a
# SoulMirror.app: DeepSeek Harness (dsh) + the soulnet-dsh plugin
# pre-installed, with a double-clickable app that boots `dsh web` and opens the
# browser. Both Apple Silicon (arm64) and Intel (x64) runtimes are shipped; the
# launcher picks the matching one via `uname -m`.
#
# macOS only (hdiutil / iconutil are macOS tools); run from a macOS shell.
# Requirements: pnpm, Go, network (nodejs.org + npm registry).
#
# Environment overrides:
#   NODE_VERSION  portable Node.js version   (default: 22.23.1)
#   DSH_VERSION   @deepseek-ai/dsh version   (default: the dsh-* version the plugin is developed against)
#   PNPM_VERSION  pnpm bundled into app/     (default: dsh/package.json packageManager)
#   SKIP_DMG=1    stage only, no .dmg (for CI smoke without hdiutil)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DSH_DIR="$ROOT/dsh"
STAGE="${SMDSH_STAGE:-/tmp/smdsh-mac-stage}"
CACHE="$ROOT/.installer-cache"
DIST="$ROOT/dist"

VERSION="$(node -p "require('$DSH_DIR/packages/dsh/package.json').version")"
DSH_VERSION="${DSH_VERSION:-$(node -p "require('$DSH_DIR/packages/dsh/package.json').devDependencies['@deepseek-ai/dsh-agent']")}"
PNPM_VERSION="${PNPM_VERSION:-$(node -p "require('$DSH_DIR/package.json').packageManager.split('@')[1]")}"
NODE_VERSION="${NODE_VERSION:-22.23.1}"

echo "== SoulMirror (DSH edition) macOS installer =="
echo "   plugin soulnet-dsh $VERSION | dsh $DSH_VERSION | node $NODE_VERSION | pnpm $PNPM_VERSION"

# ---------------------------------------------------------------- [1] plugin
echo "-- [1/7] pnpm install + build (soulnet-dsh, soulnet-dsh-sidebar)"
( cd "$DSH_DIR" && pnpm install --frozen-lockfile )
( cd "$DSH_DIR" && pnpm --filter soulnet-dsh --filter soulnet-dsh-sidebar run build )

# -------------------------------------------------------------- [2] peer x2
echo "-- [2/7] go build soulnet peer (darwin x64 + arm64)"
for arch in x64 arm64; do
  ( cd "$ROOT" && GOOS=darwin GOARCH=$arch CGO_ENABLED=0 \
      go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" \
      -o "$DSH_DIR/packages/soulnet-darwin-$arch/bin/soulnet" ./cmd/soulnet )
done

# -------------------------------------------------------------- [3] tarballs
rm -rf "$STAGE"
TARBALLS="$ROOT/.installer-stage/tarballs-mac"
rm -rf "$TARBALLS"; mkdir -p "$TARBALLS"
for pkg in soulnet-darwin-x64 soulnet-darwin-arm64 dsh sidebar; do
  ( cd "$DSH_DIR/packages/$pkg" && pnpm pack --pack-destination "$TARBALLS" >/dev/null )
done

# ------------------------------------------------------ [4] portable node x2
echo "-- [4/7] portable Node.js v$NODE_VERSION (darwin x64 + arm64)"
mkdir -p "$CACHE"
for arch in x64 arm64; do
  NODE_TGZ="$CACHE/node-v$NODE_VERSION-darwin-$arch.tar.gz"
  if [[ ! -f "$NODE_TGZ" ]]; then
    curl -fL --retry 3 -o "$NODE_TGZ.part" \
      "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-darwin-$arch.tar.gz"
    mv "$NODE_TGZ.part" "$NODE_TGZ"
  fi
done

# --------------------------------------------------------------- [5] dsh CLI
echo "-- [5/7] pnpm install @deepseek-ai/dsh@$DSH_VERSION + pnpm@$PNPM_VERSION into app/"
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
autoInstallPeers: true
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

# --------------------------------------------------------- [6] home template
echo "-- [6/7] dsh home template (pre-installed web profile)"
WEB="$STAGE/home-template/profiles/web"
mkdir -p "$WEB/node_modules"
node -e '
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
' "$WEB" "$VERSION"
cat > "$WEB/cordis.patch.yml" <<'EOF'
# Your patch layer for this dsh profile, applied after every bundle layer.
[]
EOF
cat > "$WEB/pnpm-workspace.yaml" <<'EOF'
packages:
  - .

nodeLinker: hoisted
autoInstallPeers: false
EOF

# seed node_modules from our tarballs: soulnet-dsh + both darwin peers + sidebar.
for tgz in "$TARBALLS"/soulnet-dsh-[0-9]*.tgz \
           "$TARBALLS"/soulnet-peer-darwin-x64-*.tgz \
           "$TARBALLS"/soulnet-peer-darwin-arm64-*.tgz \
           "$TARBALLS"/soulnet-dsh-sidebar-*.tgz; do
  rm -rf "$STAGE/pkg"; mkdir -p "$STAGE/pkg"
  tar -xzf "$tgz" -C "$STAGE/pkg"
  name="$(node -p "require('$STAGE/pkg/package/package.json').name")"
  mv "$STAGE/pkg/package" "$WEB/node_modules/$name"
done
rm -rf "$STAGE/pkg"
[[ -f "$WEB/node_modules/soulnet-peer-darwin-x64/bin/soulnet" ]] || {
  echo "error: darwin-x64 peer missing from template" >&2; exit 1; }
[[ -f "$WEB/node_modules/soulnet-peer-darwin-arm64/bin/soulnet" ]] || {
  echo "error: darwin-arm64 peer missing from template" >&2; exit 1; }
PLUGIN_SHA="$(shasum -a 256 "$TARBALLS"/soulnet-dsh-[0-9]*.tgz | cut -c1-16)"
printf '%s %s\n' "$VERSION" "$PLUGIN_SHA" > "$WEB/.soulmirror-template-version"

# ---------------------------------------------------------------- [7] .app
echo "-- [7/7] SoulMirror.app + .dmg"
APP="$STAGE/SoulMirror.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp installer/mac/launcher.sh "$APP/Contents/MacOS/launcher"
chmod +x "$APP/Contents/MacOS/launcher"
# Portable Node runtimes + the dsh app + home-template live under Resources.
for arch in x64 arm64; do
  NODE_TGZ="$CACHE/node-v$NODE_VERSION-darwin-$arch.tar.gz"
  mkdir -p "$APP/Contents/Resources/node-$arch-unzip"
  tar -xzf "$NODE_TGZ" -C "$APP/Contents/Resources/node-$arch-unzip" --strip-components=1
  mv "$APP/Contents/Resources/node-$arch-unzip" "$APP/Contents/Resources/node-$arch"
done
mv "$STAGE/app" "$APP/Contents/Resources/app"
mv "$STAGE/home-template" "$APP/Contents/Resources/home-template"
cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>SoulMirror</string>
  <key>CFBundleDisplayName</key><string>SoulMirror</string>
  <key>CFBundleIdentifier</key><string>cn.startupworld.soulmirror-dsh</string>
  <key>CFBundleVersion</key><string>$VERSION</string>
  <key>CFBundleShortVersionString</key><string>$VERSION</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleExecutable</key><string>launcher</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
EOF

if [[ "${SKIP_DMG:-0}" == "1" ]]; then
  echo "-- SKIP_DMG=1: staged at $STAGE, no .dmg compiled"
  exit 0
fi
mkdir -p "$DIST"
DMG="$DIST/SoulMirror-DSH-$VERSION.dmg"
rm -f "$DMG"
hdiutil create -volname "SoulMirror" -srcfolder "$APP" -ov -format UDZO "$DMG"
[[ -f "$DMG" ]] || { echo "error: expected $DMG" >&2; exit 1; }
echo "== done: $DMG ($(du -h "$DMG" | cut -f1))"
