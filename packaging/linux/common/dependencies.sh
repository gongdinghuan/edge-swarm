# Sourced by the verified package launcher. The caller checks Linux and root.

dependency_error() { printf 'swarm: %s\n' "$*" >&2; return 1; }

dependency_os_id() {
    local file=/etc/os-release line id
    [[ -r $file ]] || file=/usr/lib/os-release
    [[ -r $file ]] || return 0
    while IFS= read -r line || [[ -n $line ]]; do
        [[ $line == ID=* ]] || continue
        # Read the OS identifier as data. Never source it in the installer.
        id=${line#ID=}
        id=${id#\"}
        id=${id%\"}
        id=${id#\'}
        id=${id%\'}
        if [[ $id =~ ^[a-z0-9_.-]+$ ]]; then printf '%s\n' "$id"; fi
        return 0
    done < "$file"
}

dependency_has_ca_certificates() {
    local file
    for file in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
        [[ -r $file && -s $file ]] && return 0
    done
    return 1
}

dependency_missing_packages() {
    local mode=$1 executable
    local -a utilities=(uname sha256sum stat od tr sleep)
    if [[ $mode == systemd ]]; then
        utilities+=(install cp mv rm mktemp chmod mkdir ln readlink)
    fi
    for executable in "${utilities[@]}"; do
        if ! command -v "$executable" >/dev/null; then
            printf '%s\n' coreutils
            break
        fi
    done
    command -v bash >/dev/null || printf '%s\n' bash
    command -v curl >/dev/null || printf '%s\n' curl
    dependency_has_ca_certificates || printf '%s\n' ca-certificates
    if [[ $mode == systemd ]]; then
        command -v flock >/dev/null || printf '%s\n' util-linux
        command -v systemctl >/dev/null || printf '%s\n' systemd
    fi
}

dependency_check_systemd() {
    local release
    if ! release=$(systemctl --system --no-pager show --property=Version --value) || [[ -z $release ]]; then
        dependency_error 'systemd is not reachable; boot a systemd host for service installation or use foreground mode'
        return 1
    fi
    if [[ ! $release =~ ^v?([0-9]{1,6})([^0-9]|$) ]] || (( 10#${BASH_REMATCH[1]} < 247 )); then
        dependency_error "systemd 247 or later is required for service installation (running $release)"
        return 1
    fi
}

check_dependencies() {
    local mode=${1:-}
    local -a missing=()
    if [[ $mode != runtime && $mode != systemd ]]; then
        dependency_error 'expected dependency mode runtime or systemd'
        return 1
    fi
    mapfile -t missing < <(dependency_missing_packages "$mode")
    if ((${#missing[@]})); then
        dependency_error "missing or unusable dependencies: ${missing[*]}"
        return 1
    fi
    if [[ $mode == systemd ]]; then dependency_check_systemd; fi
}

dependency_install_packages() {
    local operation=$1 distro package variant state_version state version
    shift
    distro=$(dependency_os_id)
    case "$distro" in
        debian|ubuntu)
            if ! command -v apt-get >/dev/null; then
                dependency_error 'apt-get is unavailable; restore the distribution package manager and retry'
                return 1
            fi
            local -a apt_options=(-o Acquire::Retries=2 -o Acquire::http::Timeout=20
                -o Acquire::https::Timeout=20 -o DPkg::Lock::Timeout=60)
            if [[ $operation == install ]]; then
                if ! DEBIAN_FRONTEND=noninteractive apt-get "${apt_options[@]}" -o APT::Update::Error-Mode=any update; then
                    dependency_error 'apt repository refresh failed; check DNS, configured mirrors, or an active apt process, then retry'
                    return 1
                fi
                apt_options+=(--no-upgrade)
            else
                local -a installed=()
                for package in "$@"; do
                    if ! state_version=$(dpkg-query --show --showformat='${db:Status-Status} ${Version}' -- "$package"); then
                        dependency_error "cannot identify installed version of $package; repair the package manually"
                        return 1
                    fi
                    read -r state version <<< "$state_version"
                    if [[ $state != installed || -z $version ]]; then
                        dependency_error "cannot identify installed version of $package; repair the package manually"
                        return 1
                    fi
                    installed+=("$package=$version")
                done
                # Use the installed version so a repair does not upgrade the OS package.
                set -- "${installed[@]}"
            fi
            if ! DEBIAN_FRONTEND=noninteractive apt-get "${apt_options[@]}" --no-install-recommends --no-remove -y "$operation" "$@"; then
                dependency_error "apt dependency $operation failed; resolve the reported package or lock error and retry"
                return 1
            fi
            ;;
        rhel|rocky|almalinux)
            if ! command -v dnf >/dev/null; then
                dependency_error 'dnf is unavailable; restore the distribution package manager and retry'
                return 1
            fi
            local -a rpm_packages=()
            for package in "$@"; do
                # Retain the variant selected by the OS image if already present.
                case "$package" in
                    coreutils|curl)
                        variant=curl-minimal
                        [[ $package != coreutils ]] || variant=coreutils-single
                        if rpm -q "$variant" >/dev/null 2>&1; then package=$variant; fi
                        ;;
                esac
                rpm_packages+=("$package")
            done
            if ! dnf --setopt=timeout=20 --setopt=retries=3 --setopt=exit_on_lock=True \
                --setopt=install_weak_deps=False -y "$operation" "${rpm_packages[@]}"; then
                dependency_error "dnf dependency $operation failed; check enabled repositories, network access, and package locks, then retry"
                return 1
            fi
            ;;
        *) dependency_error "automatic dependency installation is unavailable for ${distro:-this OS}; install these packages and retry: $*"; return 1 ;;
    esac
}

ensure_dependencies() {
    local mode=${1:-}
    local -a missing=()
    if [[ $mode != runtime && $mode != systemd ]]; then
        dependency_error 'expected dependency mode runtime or systemd'
        return 1
    fi
    if [[ $mode == systemd ]]; then
        # Detect containers and unsupported service managers before changing any packages.
        if command -v systemctl >/dev/null; then
            dependency_check_systemd || return 1
        elif [[ ! -d /run/systemd/system ]]; then
            dependency_error 'systemd is not running; use a systemd host for service installation or use foreground mode'
            return 1
        fi
    fi
    mapfile -t missing < <(dependency_missing_packages "$mode")
    if ((${#missing[@]})); then
        printf 'Installing missing dependencies from configured OS repositories: %s\n' "${missing[*]}"
        dependency_install_packages install "${missing[@]}" || return 1
        hash -r
        mapfile -t missing < <(dependency_missing_packages "$mode")
        if ((${#missing[@]})); then
            printf 'Repairing dependencies that remain unavailable: %s\n' "${missing[*]}"
            dependency_install_packages reinstall "${missing[@]}" || return 1
            hash -r
        fi
        if ! check_dependencies "$mode"; then
            dependency_error 'dependencies are still unavailable; repair the reported OS packages or CA trust store, then retry'
            return 1
        fi
    fi
    printf 'Swarm %s dependencies are ready.\n' "$mode"
}
