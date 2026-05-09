#!/bin/sh
# obscura-resy.sh — Resy-snipe Signer that uses Obscura
# (https://github.com/h4ckf0r0day/obscura) to mint PerimeterX cookies
# by visiting widgets.resy.com under a stealth-mode headless browser.
#
# Contract (matches internal/resy/sign/printing_press.go):
#   $1 is the subcommand: "sign" or "reset"
#   On "sign": emit `{"headers": {"Cookie": "..."}}` on stdout, exit 0
#   On "reset": invalidate any cached cookie file, exit 0
#
# Setup:
#   1. Install obscura per https://github.com/h4ckf0r0day/obscura#install
#      Make sure `obscura` is on $PATH, OR set OBSCURA_BIN=/path/to/obscura.
#   2. (Optional) tune via env:
#        OBSCURA_TARGET_URL  — page to visit (default: widgets.resy.com)
#        OBSCURA_CACHE_PATH  — cookie cache file (default: /tmp/.obscura-resy-cookies)
#        OBSCURA_TIMEOUT     — fetch timeout seconds (default: 20)
#   3. Point resy-snipe at this script:
#        export RESY_SNIPE_SIGNER_BIN=$(pwd)/signers/obscura-resy.sh
#
# Caveats:
#   - PerimeterX cookies have a TTL (typically 5-60 min). The cache is a
#     soft optimisation; resy-snipe's Reset() invalidates it on
#     ErrAntiBotChallenge so the next Sign call refetches.
#   - Obscura's User-Agent must align with what resy-snipe sends. The
#     resy adapter uses a desktop Chrome UA (see internal/resy/client.go
#     `DefaultUserAgent`). Obscura's stealth mode also presents Chrome,
#     so this usually lines up; if you change the resy adapter UA you
#     should also pin obscura to the same string.
#   - This script is NOT a guarantee against PerimeterX. PX rotates its
#     defenses; expect to iterate. If it stops working, run
#     `obscura fetch ... --stealth --dump html` against the target URL
#     and see what's coming back.

set -eu

OBSCURA_BIN="${OBSCURA_BIN:-obscura}"
TARGET_URL="${OBSCURA_TARGET_URL:-https://widgets.resy.com/}"
CACHE_PATH="${OBSCURA_CACHE_PATH:-/tmp/.obscura-resy-cookies}"
TIMEOUT="${OBSCURA_TIMEOUT:-20}"

die() {
    # Errors go to stderr — the resy adapter logs them and falls back to
    # best-effort signing. stdout is reserved for the JSON contract.
    echo "obscura-resy: $*" >&2
    exit 1
}

# fetch_cookies runs obscura against TARGET_URL, lets PerimeterX's JS
# run for a moment, and emits the resulting document.cookie value as a
# bare string. The expression is wrapped to never fail the eval (an
# uncaught throw would kill the script and produce no output).
fetch_cookies() {
    if ! command -v "$OBSCURA_BIN" >/dev/null 2>&1; then
        die "$OBSCURA_BIN not on PATH; install per https://github.com/h4ckf0r0day/obscura#install"
    fi

    # --stealth: anti-fingerprinting that PX checks for.
    # --wait-until networkidle0: let PX's JS finish setting cookies.
    # --eval document.cookie: extract the Cookie-header-shaped string.
    # --quiet: keep stdout clean for our JSON envelope.
    "$OBSCURA_BIN" fetch "$TARGET_URL" \
        --stealth \
        --wait-until networkidle0 \
        --timeout "$TIMEOUT" \
        --eval "document.cookie" \
        --quiet 2>/dev/null
}

# emit_headers takes a cookie string ("k=v; k=v; ...") and prints the
# JSON envelope resy-snipe expects. We pass the cookies straight through
# as a Cookie header — the resy adapter merges it into outbound
# requests via the sign-and-retry path.
emit_headers() {
    cookies="$1"
    if [ -z "$cookies" ]; then
        # Empty cookie set is a real signal: PX didn't set anything,
        # which usually means stealth mode wasn't enabled or the page
        # didn't trigger the challenge. Surface as a soft no-op.
        printf '{"headers": {}}\n'
        return
    fi
    # Escape any embedded double-quotes in the cookie value before
    # interpolating into the JSON. POSIX shell substitution.
    escaped=$(printf '%s' "$cookies" | sed 's/"/\\"/g')
    printf '{"headers": {"Cookie": "%s"}}\n' "$escaped"
}

case "${1:-}" in
    sign)
        # Cache hit: re-emit the prior cookie set. The Subprocess Signer
        # in Go also caches per-process, so this file cache only helps
        # across consecutive resy-snipe invocations (common during
        # iteration / login + snipe pairs).
        if [ -f "$CACHE_PATH" ]; then
            cached=$(cat "$CACHE_PATH")
            if [ -n "$cached" ]; then
                emit_headers "$cached"
                exit 0
            fi
        fi

        cookies=$(fetch_cookies)
        # obscura --eval wraps strings in quotes; strip them.
        cookies=$(printf '%s' "$cookies" | sed 's/^"//; s/"$//')

        # Persist for next invocation.
        printf '%s' "$cookies" > "$CACHE_PATH" 2>/dev/null || true

        emit_headers "$cookies"
        ;;
    reset)
        rm -f "$CACHE_PATH" 2>/dev/null || true
        # No stdout output on reset — the contract is "exit 0 with empty
        # stdout signals cache discard." A subsequent sign call will
        # refetch.
        ;;
    "")
        die "usage: $0 sign|reset [args...]"
        ;;
    *)
        # Unknown subcommands are not an error — future resy-snipe
        # contracts may pass extra positional args (e.g. the request
        # path). Default to acting like 'sign' for forward-compat with
        # the contract documented in printing_press.go.
        case "$1" in
            sign*) exec "$0" sign "$@" ;;
            *) die "unknown subcommand: $1" ;;
        esac
        ;;
esac
