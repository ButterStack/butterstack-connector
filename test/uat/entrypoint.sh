#!/bin/sh
# UAT-ONLY entrypoint for docker-compose.uat-connector.yml's `connector`
# service (connector.spec.js, issue #1574/#1575).
#
# The daemon itself NEVER reads a credential from an environment variable --
# see connector/PROTOCOL.md section 5 ("Credential custody") and
# connector/internal/config/config.go's doc comment: every secret comes from
# connector.yml or a *_file path it names, full stop, no env fallback, no
# flag, no remote configuration. That rule is what makes "your credentials
# never leave your network" depend on the studio's file permissions rather
# than on our good behaviour, and it is enforced in code, not by convention.
#
# So this script does NOT hand the daemon a credential via the environment.
# It renders /etc/butterstack/connector.yml FROM the environment and then
# execs the real, unmodified daemon, which reads only that file -- this is
# the container-native form of "the studio's vault (or config-management
# push, or hand-authored file) injects connector.yml"; the studio's actual
# mechanism for getting bytes onto disk is out of scope for the protocol, and
# so is this script's mechanism. Both produce the same artifact: a 0600 file
# on local disk.
set -eu

umask 077

CONFIG_PATH="${UAT_CONNECTOR_CONFIG_PATH:-/etc/butterstack/connector.yml}"
mkdir -p "$(dirname "$CONFIG_PATH")"

: "${UAT_CONNECTOR_ENDPOINT:?UAT_CONNECTOR_ENDPOINT is required (wss://mock-broker:9443/connect)}"
: "${UAT_CONNECTOR_TOKEN:?UAT_CONNECTOR_TOKEN is required (bsc_<id>_<base32 secret>)}"

# depot_scope is a comma-separated list in the env, rendered as a YAML
# sequence. Default matches the UAT fixtures' seeded depot.
depot_scope_yaml=""
old_ifs="$IFS"
IFS=','
for p in ${UAT_CONNECTOR_DEPOT_SCOPE:-//depot/game/}; do
  depot_scope_yaml="${depot_scope_yaml}    - ${p}
"
done
IFS="$old_ifs"

teamcity_enabled="${UAT_CONNECTOR_TEAMCITY_ENABLED:-true}"
if [ "$teamcity_enabled" = "true" ]; then
  : "${UAT_CONNECTOR_TEAMCITY_TOKEN:?UAT_CONNECTOR_TEAMCITY_TOKEN is required when teamcity is enabled}"
fi

cat > "$CONFIG_PATH" <<YAML
endpoint: ${UAT_CONNECTOR_ENDPOINT}
endpoint_ca_file: ${UAT_CONNECTOR_ENDPOINT_CA_FILE:-/tls/ca.pem}
token: ${UAT_CONNECTOR_TOKEN}
connector_id: ${UAT_CONNECTOR_CONNECTOR_ID:-uat-connector}
log_dir: ${UAT_CONNECTOR_LOG_DIR:-/var/log/connector}
max_concurrent: ${UAT_CONNECTOR_MAX_CONCURRENT:-4}
scopes:
  depot_scope:
${depot_scope_yaml}perforce:
  enabled: ${UAT_CONNECTOR_P4_ENABLED:-true}
  binary: ${UAT_CONNECTOR_P4_BINARY:-/usr/local/bin/p4}
  port: ${UAT_CONNECTOR_P4_PORT:-ssl:p4.lan:1666}
  user: ${UAT_CONNECTOR_P4_USER:-butterstack-ro}
  ticket: ${UAT_CONNECTOR_P4_TICKET:-uat-p4-ticket-do-not-egress}
teamcity:
  enabled: ${teamcity_enabled}
  url: ${UAT_CONNECTOR_TEAMCITY_URL:-http://teamcity-stub:8111}
  token: ${UAT_CONNECTOR_TEAMCITY_TOKEN:-}
  allow_insecure_tls: false
YAML

# 0600 or the daemon refuses to start (config.go checkSecretFileMode) --
# belt and braces on top of umask 077.
chmod 0600 "$CONFIG_PATH"

exec /usr/local/bin/butterstack-connector -config "$CONFIG_PATH"
