#!/usr/bin/env bash
# Builds the two Linux packages from the binaries the release job produced:
#   dist/ncm-studio_<version>_amd64.deb   — installs to /usr/bin with a menu entry
#   dist/ncm-studio-<version>-x86_64.AppImage — one file, no install, runs anywhere
#
# Everything here is done with tools that are on a stock Ubuntu runner (ar, tar,
# zstd or gzip via dpkg-deb, curl), so the pipeline does not depend on a
# packaging framework being available.
set -Eeuo pipefail

VERSION="${VERSION:?set VERSION}"
# A tag may arrive as v1.2.3; every field in a .deb that carries a version has to
# start with a digit, so the leading v comes off here rather than being
# remembered at each call site.
VERSION="${VERSION#v}"
DIST="${DIST:-dist}"
NAME="ncm-studio"
WORK="${WORK:-build-linux}"

log() { printf '\033[1m==> %s\033[0m\n' "$*"; }

rm -rf "$WORK"
mkdir -p "$WORK" "$DIST"

# The icon is generated from source, so it is the same in every build.
log "icon"
go run ./packaging/icon "$WORK/icon-256.png" 256
go run ./packaging/icon "$WORK/icon-512.png" 512

# ── .deb ─────────────────────────────────────────────────────────────────────
# A binary-only package: one static binary, one desktop entry, one icon. The
# control file is written by hand because that is the whole of the format that
# matters here, and hand-writing it keeps the build readable.
log "deb: staging"
PKG="$WORK/deb"
mkdir -p "$PKG/DEBIAN" \
         "$PKG/usr/bin" \
         "$PKG/usr/share/applications" \
         "$PKG/usr/share/icons/hicolor/256x256/apps" \
         "$PKG/usr/share/doc/$NAME"

install -m 0755 "$DIST/ncm-studio-linux-amd64" "$PKG/usr/bin/$NAME"
install -m 0644 packaging/ncm-studio.desktop "$PKG/usr/share/applications/$NAME.desktop"
install -m 0644 "$WORK/icon-256.png" "$PKG/usr/share/icons/hicolor/256x256/apps/$NAME.png"

# Debian policy wants a changelog and a copyright file; both are generated so
# they cannot drift from the version being packaged.
cat > "$PKG/usr/share/doc/$NAME/changelog.Debian" <<EOF
$NAME ($VERSION) unstable; urgency=medium

  * Release $VERSION.

 -- ncm-studio <noreply@example.invalid>  $(date -uR)
EOF
gzip -9n "$PKG/usr/share/doc/$NAME/changelog.Debian"
cat > "$PKG/usr/share/doc/$NAME/copyright" <<'EOF'
ncm-studio is released into the public domain by its author.
The Go standard library it links against is covered by the Go licence.
EOF

INSTALLED_SIZE=$(du -sk "$PKG/usr" | cut -f1)
cat > "$PKG/DEBIAN/control" <<EOF
Package: $NAME
Version: $VERSION
Section: sound
Priority: optional
Architecture: amd64
Installed-Size: $INSTALLED_SIZE
Maintainer: ncm-studio <noreply@example.invalid>
Description: NetEase Cloud Music .ncm decryptor with a web UI
 Decrypts .ncm files into tagged MP3/FLAC, fetching lyrics and cover art on the
 way. Serves a single-page UI on 127.0.0.1:8080 and does everything locally:
 nothing is uploaded, and no account is needed.
EOF

log "deb: building"
dpkg-deb --root-owner-group --build "$PKG" "$DIST/${NAME}_${VERSION}_amd64.deb"
dpkg-deb --info "$DIST/${NAME}_${VERSION}_amd64.deb" | head -12

# ── AppImage ────────────────────────────────────────────────────────────────
# The AppDir is assembled here; appimagetool only wraps it. That is deliberate:
# the layout is the part worth reading, and a third-party action would hide it.
log "appimage: staging"
APPDIR="$WORK/${NAME}.AppDir"
mkdir -p "$APPDIR/usr/bin" "$APPDIR/usr/share/applications" \
         "$APPDIR/usr/share/icons/hicolor/256x256/apps" \
         "$APPDIR/usr/share/icons/hicolor/512x512/apps"

install -m 0755 "$DIST/ncm-studio-linux-amd64" "$APPDIR/usr/bin/$NAME"
install -m 0644 packaging/ncm-studio.desktop "$APPDIR/$NAME.desktop"
install -m 0644 packaging/ncm-studio.desktop "$APPDIR/usr/share/applications/$NAME.desktop"
install -m 0644 "$WORK/icon-256.png" "$APPDIR/$NAME.png"
install -m 0644 "$WORK/icon-256.png" "$APPDIR/usr/share/icons/hicolor/256x256/apps/$NAME.png"
install -m 0644 "$WORK/icon-512.png" "$APPDIR/usr/share/icons/hicolor/512x512/apps/$NAME.png"

# AppRun is the entry point an AppImage runs. It opens the UI in a browser and
# then becomes the server, so a double-click behaves like a launcher rather than
# like a terminal program nobody can find.
cat > "$APPDIR/AppRun" <<'RUN'
#!/bin/sh
# ncm-studio AppImage entry point.
set -e
HERE="$(dirname "$(readlink -f "$0")")"
PORT="${NCM_STUDIO_PORT:-8080}"

if command -v xdg-open >/dev/null 2>&1; then
  # Opened once the server is actually listening, so the first paint is not an
  # error page. It costs the browser tab a moment; it costs the user nothing.
  ( sleep 2; xdg-open "http://127.0.0.1:$PORT" >/dev/null 2>&1 || true ) &
fi

exec "$HERE/usr/bin/ncm-studio" -port "$PORT" "$@"
RUN
chmod 0755 "$APPDIR/AppRun"

log "appimage: fetching appimagetool"
APPIMAGE_TOOL="$WORK/appimagetool"
if ! curl -fsSL --retry 3 -o "$APPIMAGE_TOOL" \
      https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage; then
  echo "could not download appimagetool" >&2
  exit 1
fi
chmod +x "$APPIMAGE_TOOL"
# --appimage-extract-and-run: the runner has no FUSE, and an AppImage that
# refuses to start is a worse failure than one that runs slower once.
export ARCH=x86_64
export VERSION
"$APPIMAGE_TOOL" --appimage-extract-and-run "$APPDIR" \
  "$DIST/${NAME}-${VERSION}-x86_64.AppImage"

log "artifacts"
ls -lh "$DIST"/*.deb "$DIST"/*.AppImage
