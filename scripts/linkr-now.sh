#!/bin/sh
set -eu

fail() {
    printf 'linkr-now-skill: %s\n' "$*" >&2
    printf '%s\n' 'For offline use, download the complete platform ZIP from the GitHub release, extract it, and run its bundled launcher.' >&2
    exit 1
}

script_dir=$(CDPATH= cd -P "$(dirname "$0")" && pwd) || fail 'Cannot locate the skill directory.'
skill_dir=$(CDPATH= cd -P "$script_dir/.." && pwd) || fail 'Cannot locate the skill directory.'
pin=$(awk '
    NR == 1 {
        if ($0 !~ /^v[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$/) exit 1
        version = $0
    }
    NR > 1 { exit 1 }
    END {
        if (NR != 1 || version == "") exit 1
        print version
    }
' "$skill_dir/VERSION" 2>/dev/null) || fail 'VERSION must contain exactly one pinned release version, such as v0.1.0.'

case $(uname -s) in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail 'This launcher supports Linux and macOS. Use the matching launcher from a complete platform ZIP.' ;;
esac
case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail 'This launcher supports amd64 and arm64 processors.' ;;
esac
platform=$os-$arch

binary_valid() {
    [ -f "$1" ] && [ -x "$1" ] || return 1
    reported_version=$("$1" --version 2>/dev/null) || return 1
    [ "$reported_version" = "linkr-now $pin" ]
}

bundled_binary=$skill_dir/bin/$platform/linkr-now
if binary_valid "$bundled_binary"; then
    exec "$bundled_binary" "$@"
fi

if [ -n "${LINKR_NOW_CACHE_DIR:-}" ]; then
    cache_base=$LINKR_NOW_CACHE_DIR
elif [ "$os" = darwin ]; then
    [ -n "${HOME:-}" ] || fail 'HOME is required to locate the download cache.'
    cache_base=$HOME/Library/Caches/linkr-now-skill
elif [ -n "${XDG_CACHE_HOME:-}" ]; then
    cache_base=$XDG_CACHE_HOME/linkr-now-skill
else
    [ -n "${HOME:-}" ] || fail 'HOME or XDG_CACHE_HOME is required to locate the download cache.'
    cache_base=$HOME/.cache/linkr-now-skill
fi
cache_dir=$cache_base/$pin/$platform
cache_binary=$cache_dir/linkr-now
if binary_valid "$cache_binary"; then
    exec "$cache_binary" "$@"
fi
mkdir -p "$cache_dir" || fail 'Cannot create the download cache.'
cache_dir=$(CDPATH= cd -P "$cache_dir" && pwd) || fail 'Cannot access the download cache.'
cache_binary=$cache_dir/linkr-now
lock_dir=$cache_dir/.install.lock
lock_held=false
staging_dir=

cleanup() {
    if [ -n "$staging_dir" ]; then
        rm -rf "$staging_dir" 2>/dev/null || :
        staging_dir=
    fi
    if [ "$lock_held" = true ]; then
        rmdir "$lock_dir" 2>/dev/null || :
        lock_held=false
    fi
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

waited=0
while ! mkdir "$lock_dir" 2>/dev/null; do
    if binary_valid "$cache_binary"; then
        exec "$cache_binary" "$@"
    fi
    [ "$waited" -lt 30 ] || fail "Installation lock is still held at $lock_dir. If no installation is running, remove this lock directory and retry."
    sleep 1
    waited=$((waited + 1))
done
lock_held=true
# Another process may have completed the same installation while we waited.
if binary_valid "$cache_binary"; then
    cleanup
    trap - 0 HUP INT TERM
    exec "$cache_binary" "$@"
fi

if command -v curl >/dev/null 2>&1; then
    downloader=curl
elif command -v wget >/dev/null 2>&1; then
    downloader=wget
else
    fail 'Install curl or wget to download the pinned command-line tool.'
fi
if command -v sha256sum >/dev/null 2>&1; then
    checksum_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    checksum_tool=shasum
elif command -v openssl >/dev/null 2>&1; then
    checksum_tool=openssl
else
    fail 'Install sha256sum, shasum, or openssl to verify the download.'
fi

download() {
    if [ "$downloader" = curl ]; then
        curl --fail --location --silent --show-error --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 120 --output "$2" "$1"
    else
        wget -q -T 120 -O "$2" "$1"
    fi
}

staging_dir=$(mktemp -d "$cache_dir/.install.XXXXXXXX") || fail 'Cannot create a download staging directory.'
asset=linkr-now-$pin-$platform
release_url=https://github.com/radxa-linkr/linkr-now-skill/releases/download/$pin
staging_binary=$staging_dir/linkr-now
printf 'linkr-now-skill: Downloading %s for %s...\n' "$pin" "$platform" >&2
download "$release_url/$asset" "$staging_binary" || fail 'Could not download the pinned command-line tool.'
download "$release_url/SHA256SUMS" "$staging_dir/SHA256SUMS" || fail 'Could not download release checksums.'

expected_checksum=$(awk -v asset="$asset" '
    {
        filename = $2
        sub(/^[*]/, "", filename)
        if (filename == asset) {
            count++
            if (NF != 2 || length($1) != 64 || $1 !~ /^[0-9A-Fa-f]+$/) invalid = 1
            checksum = tolower($1)
        }
    }
    END {
        if (count != 1 || invalid) exit 1
        print checksum
    }
' "$staging_dir/SHA256SUMS") || fail 'The release must contain exactly one valid checksum for this platform asset.'
case $checksum_tool in
    sha256sum) checksum_output=$(sha256sum "$staging_binary") || fail 'Could not calculate the download checksum.' ;;
    shasum) checksum_output=$(shasum -a 256 "$staging_binary") || fail 'Could not calculate the download checksum.' ;;
    openssl) checksum_output=$(openssl dgst -sha256 "$staging_binary") || fail 'Could not calculate the download checksum.' ;;
esac
if [ "$checksum_tool" = openssl ]; then
    actual_checksum=$(printf '%s\n' "$checksum_output" | awk '{ print tolower($NF) }')
else
    actual_checksum=$(printf '%s\n' "$checksum_output" | awk '{ print tolower($1) }')
fi
[ "$actual_checksum" = "$expected_checksum" ] || fail 'Download checksum verification failed.'
chmod 755 "$staging_binary" || fail 'Cannot make the downloaded tool executable.'
binary_valid "$staging_binary" || fail 'The downloaded tool does not report the pinned version.'
[ ! -d "$cache_binary" ] || fail 'The cached tool path is a directory.'
# Staging and the final executable share a filesystem, so rename is atomic.
mv -f "$staging_binary" "$cache_binary" || fail 'Cannot install the verified command-line tool.'
cleanup
trap - 0 HUP INT TERM
exec "$cache_binary" "$@"
