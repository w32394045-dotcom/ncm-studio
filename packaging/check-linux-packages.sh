#!/usr/bin/env bash
# Checks the packed artifacts rather than trusting the packaging step: a .deb
# whose control file disagrees with its contents, or an AppImage missing its
# entry point, are exactly the failures that reach a user.
set -Eeuo pipefail

VERSION="${VERSION:?set VERSION}"
DIST="${DIST:-dist}"
v="${VERSION#v}"
fail=0

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
ok()  { echo "ok   $*"; }
bad() { echo "FAIL $*"; fail=1; }

deb="$DIST/ncm-studio_${v}_amd64.deb"
appimage="$DIST/ncm-studio-${v}-x86_64.AppImage"

# ── .deb ────────────────────────────────────────────────────────────────────
say "checking $deb"
[ -f "$deb" ] || bad "no .deb at $deb"
if [ -f "$deb" ]; then
  dpkg-deb --info "$deb" >/dev/null && ok "control file parses"
  # The version in the control file is what a package manager compares against.
  control_version="$(dpkg-deb -f "$deb" Version)"
  [ "$control_version" = "$v" ] && ok "control Version=$control_version" \
    || bad "control Version=$control_version, expected $v"

  # List the payload and require the three things a usable package needs: the
  # binary, a menu entry, and an icon that entry names.
  dpkg-deb --contents "$deb" | awk '{print $6}' | sort > "${TMPDIR:-/tmp}/deb-files.txt"
  for want in ./usr/bin/ncm-studio ./usr/share/applications/ncm-studio.desktop \
              ./usr/share/icons/hicolor/256x256/apps/ncm-studio.png; do
    grep -qx "$want" "${TMPDIR:-/tmp}/deb-files.txt" && ok "contains $want" || bad "missing $want"
  done

  # Extract the binary and run it: a package that cannot start is not a package.
  rm -rf "${TMPDIR:-/tmp}/deb-root"
  dpkg-deb --extract "$deb" "${TMPDIR:-/tmp}/deb-root"
  "${TMPDIR:-/tmp}/deb-root/usr/bin/ncm-studio" -version >/dev/null && ok "the packaged binary runs" \
    || bad "the packaged binary does not run"
  # And it must be static: a package that needs a runtime the user may not have
  # is the failure mode this check exists for. ldd writes its "not a dynamic
  # executable" verdict to stderr, so both streams are captured here — the
  # first version of this check looked only at stdout and failed a good binary.
  if command -v ldd >/dev/null 2>&1; then
    ldd_out="$(ldd "${TMPDIR:-/tmp}/deb-root/usr/bin/ncm-studio" 2>&1 || true)"
    case "$ldd_out" in
      *"not a dynamic executable"*|*"statically linked"*) ok "statically linked" ;;
      *) bad "not statically linked: $(echo "$ldd_out" | head -3)" ;;
    esac
  fi
fi

# ── AppImage ────────────────────────────────────────────────────────────────
say "checking $appimage"
[ -f "$appimage" ] || bad "no AppImage at $appimage"
if [ -f "$appimage" ]; then
  # The AppImage format is an ELF with a squashfs payload; both are worth
  # checking, because a truncated download still looks like a file.
  head -c 4 "$appimage" | grep -q $'\x7fELF' && ok "has an ELF header" || bad "not an ELF"
  chmod +x "$appimage"

  # --appimage-extract rather than running it: the runner has no FUSE.
  rm -rf squashfs-root
  "./$appimage" --appimage-extract >/dev/null 2>&1 || bad "the AppImage could not be extracted"
  for want in squashfs-root/AppRun squashfs-root/ncm-studio.desktop \
              squashfs-root/ncm-studio.png squashfs-root/usr/bin/ncm-studio; do
    [ -e "$want" ] && ok "contains $want" || bad "missing $want"
  done
  [ -x squashfs-root/AppRun ] && ok "AppRun is executable" || bad "AppRun is not executable"
  if [ -x squashfs-root/usr/bin/ncm-studio ]; then
    ./squashfs-root/usr/bin/ncm-studio -version >/dev/null && ok "the packaged binary runs" \
      || bad "the packaged binary does not run"
  fi
  # The desktop entry has to name the binary that exists, or the menu entry
  # opens nothing.
  entry="$(grep -m1 '^Exec=' squashfs-root/ncm-studio.desktop | cut -d= -f2)"
  command -v "$entry" >/dev/null 2>&1 || [ -x "squashfs-root/usr/bin/$entry" ] \
    && ok "Exec=$entry resolves" || bad "Exec=$entry does not exist in the AppDir"
  rm -rf squashfs-root
fi

say "result"
[ "$fail" = 0 ] && echo "all packaging checks passed" || echo "packaging checks FAILED"
exit "$fail"
