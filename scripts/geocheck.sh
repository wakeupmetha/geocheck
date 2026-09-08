#!/bin/sh
set -eu

IMAGE="${GEOCHECK_IMAGE:-ghcr.io/wakeupmetha/geocheck:ing}"
REPO="${GEOCHECK_REPO:-wakeupmetha/geocheck}"
RUNTIME="${GEOCHECK_RUNTIME:-auto}"

say() { printf '%s\n' "$*" >&2; }
die() { printf 'geocheck: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

clear_screen() {
    [ -t 1 ] || return 0

    if have clear; then
        clear 2>/dev/null && return 0
    fi
    if have tput; then
        tput clear 2>/dev/null && return 0
    fi
    printf '\033[3J\033[H\033[2J'
}

run_container() {
    engine=$1
    shift

    tty_flags=""
    use_tty_stdin=0
    if [ -t 1 ]; then
        if [ -t 0 ]; then
            tty_flags="-it"
        elif [ -r /dev/tty ] && [ -w /dev/tty ]; then
            tty_flags="-it"
            use_tty_stdin=1
        fi
    fi

    net_flags=""
    if [ "$(uname -s)" = "Linux" ]; then
        net_flags="--network host"
    fi

    cap_flags="--cap-add=NET_RAW"

    pulled=0
    if ! "$engine" image inspect "$IMAGE" >/dev/null 2>&1; then
        say "→ pulling $IMAGE"
        if ! "$engine" pull "$IMAGE" >&2; then
            # Asked for this runtime specifically, so there is nowhere to fall
            # back to and saying so is the only useful thing left.
            [ "$RUNTIME" = auto ] || die "could not pull $IMAGE"
            # In auto mode there is: the release binary below is the same
            # build. This matters because a package can be unreachable for
            # reasons that have nothing to do with the user - not yet
            # published, or not public - and refusing to run at all would be a
            # poor answer to "the image is missing".
            say "→ could not pull $IMAGE; falling back to the release binary"
            return 1
        fi
        pulled=1
    fi

    trap 'discard_image "$engine" "$pulled"' INT TERM

    clear_screen

    set +e
    if [ "$use_tty_stdin" -eq 1 ]; then
        # shellcheck disable=SC2086
        "$engine" run --rm $tty_flags $net_flags $cap_flags "$IMAGE" "$@" < /dev/tty
    else
        # shellcheck disable=SC2086
        "$engine" run --rm $tty_flags $net_flags $cap_flags "$IMAGE" "$@"
    fi
    status=$?
    set -e

    trap - INT TERM
    discard_image "$engine" "$pulled"
    exit "$status"
}

discard_image() {
    [ "$2" -eq 1 ] || return 0
    [ -z "${GEOCHECK_KEEP_IMAGE:-}" ] || return 0
    "$1" rmi "$IMAGE" >/dev/null 2>&1 || true
}

fetch() {
    if have curl; then
        curl -fsSL "$1"
    else
        wget -qO- "$1"
    fi
}

platform_asset() {
    case "$(uname -s)" in
        Linux)  asset_os="linux" ;;
        Darwin) asset_os="darwin" ;;
        *)      return 1 ;;
    esac

    case "$(uname -m)" in
        x86_64 | amd64)  asset_arch="amd64" ;;
        aarch64 | arm64) asset_arch="arm64" ;;
        *)               return 1 ;;
    esac

    [ "$asset_os" = "darwin" ] && asset_arch="all"

    printf 'geocheck_%s_%s.tar.gz' "$asset_os" "$asset_arch"
}

run_download() {
    have curl || have wget || die "need curl or wget to download the release"
    have tar || die "need tar to unpack the release"

    if [ "$RUNTIME" = "binary" ]; then
        say "→ fetching the release binary"
    else
        say "→ no container runtime; fetching the release binary"
    fi

    asset=$(platform_asset) \
        || die "no published build for $(uname -s)/$(uname -m).
  Build from source instead:
      go install github.com/$REPO/cmd/geocheck@latest"

    tmp=$(mktemp -d 2>/dev/null || mktemp -d -t geocheck)
    trap 'rm -rf "$tmp"' EXIT INT TERM

    base="https://github.com/$REPO/releases/latest/download"
    say "→ downloading $asset"
    fetch "$base/$asset" > "$tmp/$asset" || die "could not download $asset from $REPO."

    if have sha256sum || have shasum; then
        if fetch "$base/checksums.txt" > "$tmp/checksums.txt" 2>/dev/null; then
            expected=$(sed -n "s/^\([0-9a-f]*\)  *$asset\$/\1/p" "$tmp/checksums.txt" | head -1)
            if [ -n "$expected" ]; then
                if have sha256sum; then
                    actual=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
                else
                    actual=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
                fi
                [ "$actual" = "$expected" ] \
                    || die "checksum mismatch for $asset; refusing to run it"
                say "→ checksum verified"
            fi
        fi
    fi

    tar -xzf "$tmp/$asset" -C "$tmp" || die "could not unpack $asset"
    [ -x "$tmp/geocheck" ] || chmod +x "$tmp/geocheck" 2>/dev/null || true
    [ -f "$tmp/geocheck" ] || die "the archive did not contain a geocheck binary"

    clear_screen
    set +e
    "$tmp/geocheck" "$@"
    status=$?
    set -e
    exit "$status"
}

usage() {
    cat >&2 <<'USAGE'
geocheck launcher

  curl -fsSL https://raw.githubusercontent.com/wakeupmetha/geocheck/main/scripts/geocheck.sh | sh
  curl -fsSL https://raw.githubusercontent.com/wakeupmetha/geocheck/main/scripts/geocheck.sh | sh -s -- [launcher options] [geocheck options]

Launcher options, which must come first and are not passed on:
  --runtime auto      docker, else podman, else the release binary (default)
  --runtime docker    require docker
  --runtime podman    require podman
  --runtime binary    require the release binary, ignoring any container runtime
  --launcher-help     this text

Environment:
  GEOCHECK_RUNTIME    same as --runtime
  GEOCHECK_IMAGE      image to run (default ghcr.io/wakeupmetha/geocheck:ing)
  GEOCHECK_REPO       GitHub repository to download releases from
  GEOCHECK_KEEP_IMAGE keep an image this run pulled instead of removing it

Everything after the launcher options goes to geocheck itself; run
`... | sh -s -- --help` for those.
USAGE
    exit 0
}

parse_options() {
    OPTS_CONSUMED=0
    while [ $# -gt 0 ]; do
        case $1 in
            --runtime=*)
                RUNTIME=${1#--runtime=}
                OPTS_CONSUMED=$((OPTS_CONSUMED + 1))
                shift
                ;;
            --runtime)
                [ $# -ge 2 ] || die "--runtime needs a value: auto, docker, podman or binary"
                RUNTIME=$2
                OPTS_CONSUMED=$((OPTS_CONSUMED + 2))
                shift 2
                ;;
            --launcher-help)
                usage
                ;;
            *)
                break
                ;;
        esac
    done
}

require() {
    have "$1" && return 0
    die "--runtime $1 was requested but $1 is not installed.
  Install it, or use --runtime binary to download the release instead."
}

main() {
    parse_options "$@"
    shift "$OPTS_CONSUMED"

    case $RUNTIME in
        docker | podman)
            require "$RUNTIME"
            run_container "$RUNTIME" "$@"
            ;;
        binary)
            run_download "$@"
            ;;
        auto) ;;
        *)
            die "unknown runtime '$RUNTIME'; use auto, docker, podman or binary"
            ;;
    esac

    for candidate in docker podman; do
        if have "$candidate"; then
            # This returns only when the image could not be pulled; on every
            # other path it runs geocheck and exits. A second runtime would
            # reach the same registry and fail the same way, so give up on
            # containers here rather than trying podman next.
            run_container "$candidate" "$@" || break
        fi
    done

    run_download "$@"
}

main "$@"