#!/usr/bin/env bash
set +x
set -euo pipefail
umask 077

fail() { printf 'swarm: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || fail "missing command $1; install $2 from your distribution and retry"; }

script=${BASH_SOURCE[0]}
[[ $script == */* ]] || script=./$script
package=$(cd -- "${script%/*}" && pwd -P)
read -r role arch < "$package/package.info" || fail 'package.info missing or invalid; re-extract the archive'
case "$role" in
    manager) binary="$package/bin/control" ;;
    worker) binary="$package/bin/agent" ;;
    *) fail 'invalid package role; re-extract the archive' ;;
esac

verify() {
    need uname coreutils
    [[ $(uname -s) == Linux ]] || fail 'this package requires Linux'
    case "$(uname -m):$arch" in
        x86_64:amd64|aarch64:arm64) ;;
        *) fail "wrong architecture: package=$arch, host=$(uname -m); download the matching archive" ;;
    esac
    need sha256sum coreutils
    (cd -- "$package" && sha256sum --strict --check --quiet SHA256SUMS) ||
        fail 'package files are missing or corrupt; verify the archive checksum and re-extract into a new directory'
    [[ -x $binary ]] || fail 'binary is not executable; preserve tar modes or restore chmod 0755, and use an executable mount'
}

load_config() {
    local file=$1 line key value mode
    local -A seen=()
    [[ -f $file && -r $file ]] || fail "configuration not readable: $file; see README.zh-CN.md"
    if [[ -z ${CREDENTIALS_DIRECTORY:-} || $file != "$CREDENTIALS_DIRECTORY/swarm.env" ]]; then
        need stat coreutils
        mode=$(stat -Lc '%a' -- "$file")
        [[ $mode == 600 || $mode == 400 ]] || fail "configuration must have mode 0600 or 0400: $file"
    fi
    unset SWARM_PORT SWARM_ADMIN_TOKEN SWARM_API_KEYS SWARM_DATABASE_URL SWARM_MODEL_ACCESS SWARM_STATE_DIR
    unset SWARM_CONTROL_URL SWARM_MODEL SWARM_BACKEND_URL SWARM_NODE_ID SWARM_NODE_TOKEN
    unset SWARM_BACKEND_KEY SWARM_BACKEND_KIND SWARM_GPU SWARM_VRAM_MIB
    while IFS= read -r line || [[ -n $line ]]; do
        line=${line%$'\r'}
        [[ -z $line || $line == \#* ]] && continue
        [[ $line == *=* ]] || fail 'invalid configuration line; use KEY=value with no shell quoting'
        key=${line%%=*}
        value=${line#*=}
        case "$role:$key" in
            manager:SWARM_PORT|manager:SWARM_ADMIN_TOKEN|manager:SWARM_API_KEYS|manager:SWARM_DATABASE_URL|manager:SWARM_MODEL_ACCESS|manager:SWARM_STATE_DIR) ;;
            worker:SWARM_CONTROL_URL|worker:SWARM_MODEL|worker:SWARM_BACKEND_URL|worker:SWARM_NODE_ID|worker:SWARM_NODE_TOKEN|worker:SWARM_BACKEND_KEY|worker:SWARM_BACKEND_KIND|worker:SWARM_GPU|worker:SWARM_VRAM_MIB) ;;
            *) fail 'unknown configuration key; compare the file with the role example' ;;
        esac
        [[ ! ${seen[$key]+present} ]] || fail "duplicate configuration key: $key"
        seen[$key]=1
        export "$key=$value"
    done < "$file"
}

init_manager() {
    local file=$1 admin api
    need od coreutils
    need tr coreutils
    admin=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
    api=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
    [[ $admin =~ ^[0-9a-f]{64}$ && $api =~ ^[0-9a-f]{64}$ && $admin != "$api" ]] || fail 'could not generate credentials'
    (set -C; printf 'SWARM_PORT=8080\nSWARM_ADMIN_TOKEN=%s\nSWARM_API_KEYS={"%s":"default"}\n' "$admin" "$api" > "$file") ||
        fail 'could not create configuration; check directory permissions and keep the existing file if present'
    printf 'Created private manager configuration at %s\n' "$file"
}

action=${1:-}
if [[ $action == install || $action == deps ]]; then
    shift
    verify
    source "$package/dependencies.sh"
    if [[ $action == deps ]]; then
        if [[ $# == 1 && $1 == --check ]]; then
            check_dependencies runtime
            exit
        fi
        [[ $# == 0 ]] || fail 'usage: swarm.sh deps [--check]'
        (( EUID == 0 )) || fail 'run sudo bash ./swarm.sh deps or use deps --check to inspect dependencies'
        ensure_dependencies runtime
        exit
    fi
    start=yes
    if [[ ${1:-} == --no-start ]]; then start=no; shift; fi
    [[ $# -le 1 ]] || fail 'usage: swarm.sh install [--no-start] [config-file]'
    (( EUID == 0 )) || fail 'run sudo bash ./swarm.sh install'
    ensure_dependencies systemd
    source "$package/installer.sh"
    install_service / "${1:-}" "$start"
    exit
fi

usage='usage: swarm.sh {verify|init|check|run|ready} [config-file], deps [--check], or install [--no-start] [config-file]'
[[ $# -le 2 ]] || fail "$usage"
config=${2:-${CREDENTIALS_DIRECTORY:+$CREDENTIALS_DIRECTORY/swarm.env}}
config=${config:-$package/$role.env}
case "$action" in
    verify) verify; printf 'Verified %s linux/%s\n' "$role" "$arch"; exit ;;
    init) [[ $role == manager ]] || fail 'copy worker.env.example and fill in the enrollment details'; init_manager "$config"; exit ;;
    check|run) verify ;;
    ready) [[ $role == manager ]] || fail 'check the worker in the management console' ;;
    *) fail "$usage" ;;
esac
load_config "$config"

args=()
[[ $action != check ]] || args+=(-check)
if [[ $role == manager ]]; then
    port=${SWARM_PORT:-8080}
    [[ $port =~ ^[0-9]{1,5}$ ]] && (( 10#$port >= 1024 && 10#$port <= 65535 )) || fail 'SWARM_PORT must be 1024–65535'
    port=$((10#$port))
    if [[ $action == ready ]]; then
        need curl curl
        need sleep coreutils
        deadline=$((SECONDS + 30))
        while (( SECONDS < deadline )); do
            if code=$(curl -q --noproxy '*' --silent --fail --max-time 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/readyz") && [[ $code == 200 ]]; then
                printf 'Manager is ready on port %s\n' "$port"
                exit
            fi
            sleep 1
        done
        fail 'manager readiness timed out; inspect manager logs and database configuration'
    fi
    [[ $action != check ]] || need curl curl
    state=${STATE_DIRECTORY:-${SWARM_STATE_DIR:-$package/state}}
    [[ $state == /* ]] || fail 'SWARM_STATE_DIR must be an absolute path'
    exec "$binary" "${args[@]}" -listen "127.0.0.1:$port" -dsn "$state/swarm.db"
fi

for key in SWARM_CONTROL_URL SWARM_MODEL SWARM_BACKEND_URL SWARM_NODE_ID SWARM_NODE_TOKEN; do
    value=${!key-}
    [[ -n $value && $value != REPLACE_ME ]] || fail "set $key in the worker configuration"
done
exec "$binary" "${args[@]}" -control "$SWARM_CONTROL_URL" -model "$SWARM_MODEL" -backend "$SWARM_BACKEND_URL" \
    -backend-kind "${SWARM_BACKEND_KIND:-openai-compatible}" -gpu "${SWARM_GPU:-}" -vram-mib "${SWARM_VRAM_MIB:-0}"
