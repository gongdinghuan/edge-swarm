#!/usr/bin/env bash
set -euo pipefail

repo=$(cd -- "${BASH_SOURCE[0]%/*}/.." && pwd -P)
temp=$(cd -- "${TMPDIR:-/tmp}" && pwd -P)
scratch=$(mktemp -d "$temp/swarm-install-qa.XXXXXX")
[[ $(cd -- "$scratch" && pwd -P) == "$temp"/swarm-install-qa.* ]]
trap 'rm -rf -- "$scratch"' EXIT
root=$scratch/root
package="$scratch/发行 测试"
mkdir -p "$package/bin" "$root"
export calls=$scratch/calls fixture_active=$scratch/active
touch "$calls"

fail() { printf '%s\n' "$*" >&2; exit 1; }
flock() { :; }
systemctl() {
    case $1 in
        is-active)
            local state=inactive
            [[ ! -e $fixture_active.${@: -1} ]] || state=active
            [[ $2 == --quiet ]] || printf '%s\n' "$state"
            [[ $state == active ]]
            ;;
        start)
            printf '%s\n' "$*" >> "$calls"
            [[ ! -e $root/start-fails ]] || return 1
            touch "$fixture_active.${@: -1}"
            ;;
        daemon-reload|enable|reset-failed) printf '%s\n' "$*" >> "$calls" ;;
        *) fail "unexpected systemctl call: $*" ;;
    esac
}
init_manager() { printf 'SWARM_PORT=8080\n' > "$1"; }

if [[ $(uname -s) == MINGW* ]]; then
    # Git Bash on NTFS cannot set the installer's POSIX directory modes.
    install() {
        local directory=no parents=no
        while (( $# )); do
            case $1 in
                -d) directory=yes; shift ;;
                -D) parents=yes; shift ;;
                -m) shift 2 ;;
                --) shift; break ;;
                *) break ;;
            esac
        done
        if [[ $directory == yes ]]; then
            mkdir -p -- "$@"
        else
            [[ $parents == no ]] || mkdir -p -- "${2%/*}"
            cp -- "$1" "$2"
        fi
    }
fi

fixture() {
    local revision=$1 exe=control
    [[ $role == manager ]] || exe=agent
    printf '#!/bin/bash\n# release %s\n' "$revision" > "$package/bin/$exe"
    printf '[Service]\n# release %s\n' "$revision" > "$package/edge-swarm-$role.service"
    printf '#!/bin/bash\n[[ ! -e ${calls}.check-fails ]]\n' > "$package/swarm.sh"
    chmod +x "$package/bin/$exe" "$package/swarm.sh"
    printf '%s amd64\n' "$role" > "$package/package.info"
    printf '# Example for %s\n' "$role" > "$package/$role.env.example"
    local -a files=("bin/$exe" "edge-swarm-$role.service" swarm.sh package.info "$role.env.example")
    (cd -- "$package" && sha256sum --text "${files[@]}" > SHA256SUMS)
}

expect_failure() {
    local message=$1 output
    shift
    if output=$( (set -e; "$@") 2>&1); then fail "expected failure: $message"; fi
    [[ $output == *"$message"* ]] || fail "expected '$message', got: $output"
}

source "$repo/packaging/linux/common/installer.sh"

role=manager
fixture 1
install_service "$root" '' yes
config=$root/etc/edge-swarm/manager.env
target=$root/opt/edge-swarm-manager
[[ -f $config && -f $target/bin/control && -e $fixture_active.edge-swarm-manager.service ]]
printf 'SWARM_PORT=9090\n' > "$config"
install_service "$root" '' yes
[[ $(< "$config") == SWARM_PORT=9090 ]]
[[ $(grep -c '^start ' "$calls") == 1 ]]

fixture 2
expect_failure 'stop edge-swarm-manager' install_service "$root" '' yes
rm -- "$fixture_active.edge-swarm-manager.service"
touch "${calls}.check-fails"
expect_failure 'preflight failed' install_service "$root" '' yes
[[ $(< "$target/bin/control") == *'release 1' ]]
rm -- "${calls}.check-fails"
touch "$root/start-fails"
expect_failure 'failed to start' install_service "$root" '' yes
[[ $(< "$config") == SWARM_PORT=9090 ]]
[[ $(< "$target/bin/control") == *'release 2' ]]
shopt -s nullglob
backups=("$root"/opt/.edge-swarm-manager-backup.*/package/bin/control)
[[ ${#backups[@]} == 1 && $(< "${backups[0]}") == *'release 1' ]]
rm -- "$root/start-fails"
install_service "$root" '' yes
[[ -e $fixture_active.edge-swarm-manager.service ]]

package=$scratch/worker-package
mkdir -p "$package/bin"
role=worker
fixture 1
install_service "$root" '' yes
config=$root/etc/edge-swarm/worker.env
[[ -f $config && ! -e $fixture_active.edge-swarm-worker.service ]]
printf 'SWARM_MODEL=demo\n' > "$config"
install_service "$root" '' no
[[ ! -e $fixture_active.edge-swarm-worker.service ]]
install_service "$root" '' yes
[[ -e $fixture_active.edge-swarm-worker.service && $(< "$config") == SWARM_MODEL=demo ]]
printf 'SWARM_MODEL=different\n' > "$scratch/another.env"
expect_failure 'will not replace' install_service "$root" "$scratch/another.env" yes
[[ $(< "$config") == SWARM_MODEL=demo ]]

printf 'PASS first install, rerun, worker enrollment, upgrade guard, backup, and retry\n'
