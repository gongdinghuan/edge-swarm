#!/usr/bin/env bash
set -euo pipefail

cd -- "${BASH_SOURCE[0]%/*}/.."
source packaging/linux/common/dependencies.sh

# The package managers and OS probes below are fixtures. No host install can run.
fixture_distro=ubuntu
ca_available=yes
missing_curl=no
missing_flock=no
repo_failure=no
install_works=yes
reinstall_works=yes
systemd_release=v252.5-1
update_calls=0
install_calls=0
reinstall_calls=0
declare -a install_args=()

dependency_os_id() { printf '%s\n' "$fixture_distro"; }
dependency_has_ca_certificates() { [[ $ca_available == yes ]]; }

command() {
    [[ $1 == -v ]] || return 1
    case "$2" in
        curl) [[ $missing_curl == no ]] ;;
        flock) [[ $missing_flock == no ]] ;;
        *) return 0 ;;
    esac
}

apt-get() {
    case " $* " in
        *' update '*) update_calls=$((update_calls + 1)); [[ $repo_failure == no ]] ;;
        *' install '*)
            install_calls=$((install_calls + 1))
            install_args=("$@")
            if [[ $install_works != no ]]; then
                ca_available=yes
                missing_flock=no
            fi
            if [[ $install_works == yes ]]; then missing_curl=no; fi
            ;;
        *' reinstall '*)
            reinstall_calls=$((reinstall_calls + 1))
            install_args=("$@")
            if [[ $reinstall_works == yes ]]; then missing_curl=no; fi
            ;;
        *) return 1 ;;
    esac
}

dnf() {
    install_args=("$@")
    if [[ " $* " == *' reinstall '* ]]; then
        reinstall_calls=$((reinstall_calls + 1))
        if [[ $reinstall_works == yes ]]; then missing_curl=no; fi
    else
        install_calls=$((install_calls + 1))
        if [[ $install_works == yes ]]; then missing_curl=no; fi
    fi
}

dpkg-query() { printf '%s' 'installed 1.2.3'; }
rpm() { [[ $* == '-q curl-minimal' ]]; }
systemctl() {
    [[ -n $systemd_release ]] || return 1
    printf '%s\n' "$systemd_release"
}

assert_args() { [[ " ${install_args[*]} " == *" $1 " ]]; }

ensure_dependencies runtime
[[ $update_calls == 0 && $install_calls == 0 ]]

ca_available=no
missing_curl=yes
if check_dependencies runtime; then exit 1; fi
[[ $install_calls == 0 ]]
ensure_dependencies runtime
[[ $update_calls == 1 && $install_calls == 1 ]]
assert_args 'curl ca-certificates'
[[ " ${install_args[*]} " != *' coreutils '* && " ${install_args[*]} " != *' util-linux '* ]]
ensure_dependencies runtime
[[ $update_calls == 1 && $install_calls == 1 ]]

missing_flock=yes
ensure_dependencies runtime
[[ $install_calls == 1 ]]
ensure_dependencies systemd
[[ $install_calls == 2 ]]
assert_args util-linux

systemd_release=246
if ensure_dependencies systemd; then exit 1; fi
systemd_release=
if ensure_dependencies systemd; then exit 1; fi
systemd_release=252.5-1
[[ $install_calls == 2 ]]

fixture_distro=unsupported
missing_curl=yes
if ensure_dependencies runtime; then exit 1; fi
[[ $install_calls == 2 ]]

fixture_distro=ubuntu
repo_failure=yes
if ensure_dependencies runtime; then exit 1; fi
[[ $install_calls == 2 ]]
repo_failure=no
install_works=partial
ca_available=no
ensure_dependencies runtime
[[ $install_calls == 3 && $reinstall_calls == 1 && $update_calls == 4 ]]
assert_args 'reinstall curl=1.2.3'
[[ " ${install_args[*]} " != *' ca-certificates'* && " ${install_args[*]} " != *' --no-upgrade '* ]]

missing_curl=yes
install_works=no
reinstall_works=no
if ensure_dependencies runtime; then exit 1; fi
[[ $install_calls == 4 && $reinstall_calls == 2 && $update_calls == 5 ]]

fixture_distro=rocky
reinstall_works=yes
ensure_dependencies runtime
[[ $install_calls == 5 && $reinstall_calls == 3 ]]
assert_args 'reinstall curl-minimal'

printf '%s\n' 'Linux dependency provisioning fixtures passed'
