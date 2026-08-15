#!/bin/sh
# Copy the client-only MCP credential into a directory the service account owns,
# then run the web process without root privileges. Compose implements file
# secrets as bind mounts locally, so the source file can retain restrictive
# host permissions without making the running application privileged.
set -eu

secret_source="${SYNCBASE_MCP_TOKEN_FILE:?SYNCBASE_MCP_TOKEN_FILE is required}"
runtime_dir=/run/syncbase
runtime_token="$runtime_dir/mcp_client_token"

umask 077
mkdir -p "$runtime_dir"
cp "$secret_source" "$runtime_token"
chown 10001:10001 "$runtime_dir" "$runtime_token"
chmod 0700 "$runtime_dir"
chmod 0400 "$runtime_token"

export SYNCBASE_MCP_TOKEN_FILE="$runtime_token"
exec setpriv --reuid=10001 --regid=10001 --clear-groups /app/syncbase "$@"
