#!/bin/sh
set -eu
umask 077

if [ "$#" -ne 1 ]; then
    printf '%s\n' 'Usage: sh deploy/control/init-secrets.sh /absolute/path/to/new-secrets-directory' >&2
    exit 2
fi
directory=$1
case "$directory" in
    /*) ;;
    *) printf '%s\n' 'Secret directory must be an absolute path.' >&2; exit 2 ;;
esac
command -v openssl >/dev/null

# mkdir refuses to overwrite a directory that already contains live secrets.
mkdir -m 0700 -- "$directory"
openssl rand -hex 32 > "$directory/postgres_password"
openssl rand -hex 32 > "$directory/swarm_admin_token"
api_key=$(openssl rand -hex 32)
printf '{"%s":"team-a"}\n' "$api_key" > "$directory/swarm_api_keys"

# Compose file secrets are individual bind mounts. Non-root service UIDs need
# read access inside their container. The host directory itself remains 0700.
chmod 0444 "$directory/postgres_password" "$directory/swarm_admin_token" "$directory/swarm_api_keys"
printf '%s\n' 'Generated secrets in the private directory. No values were printed.'
