#!/bin/sh
set -eu

for name in postgres_password swarm_api_keys swarm_admin_token; do
    if [ ! -s "/run/secrets/$name" ] || [ ! -r "/run/secrets/$name" ]; then
        printf '%s\n' "Missing or unreadable secret: $name" >&2
        exit 1
    fi
done

password=$(cat /run/secrets/postgres_password)
case "$password" in
    ''|*[!a-f0-9]*) printf '%s\n' 'PostgreSQL password must be lowercase hex.' >&2; exit 1 ;;
esac
if [ "${#password}" -lt 64 ]; then
    printf '%s\n' 'PostgreSQL password must contain at least 64 hex characters.' >&2
    exit 1
fi

export SWARM_DATABASE_URL="postgres://swarm:$password@postgres:5432/swarm?sslmode=disable"
export SWARM_API_KEYS="$(cat /run/secrets/swarm_api_keys)"
export SWARM_ADMIN_TOKEN="$(cat /run/secrets/swarm_admin_token)"
exec /usr/local/bin/control "$@"
