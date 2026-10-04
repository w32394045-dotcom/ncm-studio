#!/usr/bin/env bash
# Packages the Windows builds as one-click zips.
#
# Each zip holds the .exe, the launcher that opens the browser and starts the
# server, and a short readme in the language the launcher's user most likely
# reads. Nothing is installed by it: extracting and double-clicking is the whole
# procedure, which is the point of shipping a single static binary.
set -Eeuo pipefail

VERSION="${VERSION:?set VERSION}"
DIST="${DIST:-dist}"
WORK="${WORK:-build-windows}"

rm -rf "$WORK"
mkdir -p "$WORK"

for arch in amd64 arm64; do
  exe="$DIST/ncm-studio-windows-${arch}.exe"
  [ -f "$exe" ] || { echo "missing $exe" >&2; exit 1; }

  stage="$WORK/ncm-studio-${VERSION}-windows-${arch}"
  mkdir -p "$stage"
  install -m 0755 "$exe" "$stage/ncm-studio.exe"
  install -m 0644 packaging/windows/start-ncm-studio.bat "$stage/start-ncm-studio.bat"
  install -m 0644 packaging/windows/README-zh.txt "$stage/说明.txt"
  install -m 0644 README.md "$stage/README.md"

  # Zip from inside the directory so the archive has no leading path component:
  # extracting it gives a folder of files rather than a folder in a folder.
  ( cd "$WORK" && zip -q -r -9 "../$DIST/ncm-studio-${VERSION}-windows-${arch}.zip" \
      "ncm-studio-${VERSION}-windows-${arch}" )
  echo "built $DIST/ncm-studio-${VERSION}-windows-${arch}.zip"
done

ls -lh "$DIST"/*.zip
