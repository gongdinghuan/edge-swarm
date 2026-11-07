# Sourced by swarm.sh after verifying the extracted package.

stage_release() {
    local stage=$1 digest relative mode
    while read -r digest relative; do
        [[ $digest =~ ^[a-f0-9]{64}$ && $relative =~ ^[a-zA-Z0-9_.-]+(/[a-zA-Z0-9_.-]+)*$ ]] &&
            [[ /$relative/ != */../* && /$relative/ != */./* ]] || fail 'invalid release manifest path'
        [[ -f $package/$relative && ! -L $package/$relative ]] || fail "expected a regular package file: $relative"
        mode=0644
        case "$relative" in bin/*|swarm.sh) mode=0755 ;; esac
        (umask 022; install -D -m "$mode" -- "$package/$relative" "$stage/$relative") || fail "could not stage $relative"
    done < "$package/SHA256SUMS"
    install -m 0644 -- "$package/SHA256SUMS" "$stage/SHA256SUMS" || fail 'could not stage SHA256SUMS'
    (cd -- "$stage" && sha256sum --strict --check --quiet SHA256SUMS) || fail 'staged release failed integrity check'
    chmod 0755 -- "$stage" || fail 'could not make the release directory readable'
}

install_service() (
    local root=${1%/} import=$2 start=$3 lockfd
    local name=edge-swarm-$role unit=edge-swarm-$role.service
    local cfgdir=$root/etc/edge-swarm unitdir=$root/etc/systemd/system lockdir=$root/run/edge-swarm-install
    local opt=$root/opt config=$cfgdir/$role.env unitfile=$unitdir/$unit
    local target=$opt/$name executable=control
    [[ $role == manager ]] || executable=agent
    local release_stage= config_stage= unit_stage= backup= same=no active=no pending=no path state

    trap '[[ -z $release_stage ]] || rm -rf -- "$release_stage"; [[ -z $config_stage ]] || rm -rf -- "$config_stage"; [[ -z $unit_stage ]] || rm -f -- "$unit_stage"' EXIT
    for path in "$cfgdir" "$lockdir" "$config" "$target" "$unitfile"; do
        [[ ! -L $path ]] || fail "unexpected symlink at $path; inspect the existing installation"
    done
    install -d -m 0700 -- "$lockdir" || fail "could not create $lockdir"
    [[ ! -L $lockdir/install.lock ]] || fail 'unexpected installer lock symlink'
    exec {lockfd}>"$lockdir/install.lock" || fail 'could not open the installer lock'
    flock -w 30 "$lockfd" || fail 'another Swarm install is running; retry after it finishes'

    [[ ! -e $target || -d $target ]] || fail "expected a directory at $target"
    [[ ! -e $config || -f $config ]] || fail "expected a regular configuration file at $config"
    [[ ! -e $unitfile || -f $unitfile ]] || fail "expected a regular service file at $unitfile"
    if [[ -x $target/bin/$executable && -f $target/SHA256SUMS && $(< "$target/SHA256SUMS") == "$(< "$package/SHA256SUMS")" ]] &&
        (cd -- "$target" && sha256sum --strict --check --quiet SHA256SUMS 2>/dev/null); then
        same=yes
    fi
    state=$(systemctl is-active "$unit" 2>/dev/null) || :
    case $state in
        active|reloading)
            active=yes
            [[ $same == yes ]] || fail "stop $name before replacing its release: sudo systemctl stop $name"
            ;;
        inactive|failed|unknown) ;;
        *) fail "could not install while $name state is ${state:-unavailable}; check systemctl status $name" ;;
    esac

    [[ -z $import || ( -f $import && -r $import ) ]] || fail "cannot read configuration file: $import"
    install -d -m 0700 -- "$cfgdir" || fail "could not create $cfgdir"
    if [[ -f $config ]]; then
        [[ -z $import || $(< "$import") == "$(< "$config")" ]] || fail "will not replace $config; use sudoedit to update it"
    else
        config_stage=$(mktemp -d "$cfgdir/.${role}.XXXXXX") || fail 'could not stage configuration'
        if [[ -n $import || $role == worker ]]; then
            install -m 0600 -- "${import:-$package/$role.env.example}" "$config_stage/config" || fail 'could not copy configuration'
        else
            init_manager "$config_stage/config" >/dev/null
        fi
        mv -T -- "$config_stage/config" "$config" || fail 'could not publish configuration'
    fi
    if [[ $role == worker && $(< "$config") == "$(< "$package/$role.env.example")" ]]; then
        pending=yes
    fi
    if [[ $start == yes && $active == no && $pending == no ]]; then
        bash "$package/swarm.sh" check "$config" || fail "preflight failed; correct $config or repair dependencies, then retry install"
    fi

    if [[ $same == no ]]; then
        [[ -d $opt ]] || install -d -m 0755 -- "$opt" || fail "could not create $opt"
        release_stage=$(mktemp -d "$opt/.$name-stage.XXXXXX") || fail 'could not stage release'
        stage_release "$release_stage"
    fi
    if [[ ! -f $unitfile || $(< "$unitfile") != "$(< "$package/$unit")" ]]; then
        [[ -d $unitdir ]] || install -d -m 0755 -- "$unitdir" || fail "could not create $unitdir"
        unit_stage=$(mktemp "$unitdir/.$unit.XXXXXX") || fail 'could not stage service definition'
        install -m 0644 -- "$package/$unit" "$unit_stage" || fail 'could not copy service definition'
    fi
    if [[ $same == no ]]; then
        if [[ -d $target ]]; then
            backup=$(mktemp -d "$opt/.$name-backup.XXXXXX") || fail 'could not create release backup'
            if [[ -f $unitfile ]]; then
                install -m 0644 -- "$unitfile" "$backup/$unit" || fail 'could not back up the service definition'
            fi
            mv -T -- "$target" "$backup/package" || fail 'could not move the previous release into its backup'
            printf 'Previous release saved in %s\n' "$backup"
        fi
        if ! mv -T -- "$release_stage" "$target"; then
            if [[ -n $backup ]]; then
                mv -T -- "$backup/package" "$target" || fail "could not restore release; previous files are in $backup"
            fi
            fail 'could not publish the staged release'
        fi
        release_stage=
    fi
    if [[ -n $unit_stage ]]; then
        mv -T -- "$unit_stage" "$unitfile" || fail 'could not publish service definition; rerun install'
        unit_stage=
    fi
    systemctl daemon-reload || fail 'systemd reload failed; rerun install when systemd is available'

    if [[ $pending == yes ]]; then
        printf 'Worker installed. Set enrollment and backend values with sudoedit %s\n' "$config"
        printf 'Then run sudo bash %s/swarm.sh install\n' "$target"
    elif [[ $start == no ]]; then
        printf 'Installed %s. Configure %s before starting.\n' "$name" "$config"
    else
        if [[ $active == no ]]; then
            systemctl reset-failed "$unit" 2>/dev/null || true
            systemctl start "$unit" || fail "failed to start $name; inspect journalctl -u $name, then retry install"
            systemctl is-active --quiet "$unit" || fail "failed to start $name; inspect journalctl -u $name, then retry install"
        fi
        systemctl enable "$unit" || fail "could not enable $name at boot; rerun install"
        printf 'Installed and started %s. Configuration: %s\n' "$name" "$config"
        if [[ $role == worker ]]; then
            printf 'Confirm that the node is ready in the management console.\n'
        fi
    fi
    :
)
