#!/bin/sh
# Prepares a deployment from the settings jerboa-infra (Terraform) keeps in
# the GitHub environment: fills the environment's IDs into wrangler.toml and
# writes the secrets file for wrangler deploy --secrets-file.
#
#   REGISTRY_D1_DATABASE_ID=… REGISTRY_GITHUB_CLIENT_ID=… REGISTRY_JWT_SIGNING_KEY=… \
#     scripts/configure-env.sh staging "$RUNNER_TEMP/secrets.json"
set -eu

usage() { echo "usage: $0 staging|production <secrets.json>" >&2; exit 2; }
[ $# -eq 2 ] || usage
case $1 in
staging) placeholder=STAGING ;;
production) placeholder=PRODUCTION ;;
*) usage ;;
esac
secrets=$2

missing() {
  echo "::error::$1 is not set: apply jerboa-infra, which sets it in the GitHub environment" >&2
  exit 1
}

d1=${REGISTRY_D1_DATABASE_ID:-}
client=${REGISTRY_GITHUB_CLIENT_ID:-}
key=${REGISTRY_JWT_SIGNING_KEY:-}
[ -n "$d1" ] || missing REGISTRY_D1_DATABASE_ID
[ -n "$client" ] || missing REGISTRY_GITHUB_CLIENT_ID
[ -n "$key" ] || missing REGISTRY_JWT_SIGNING_KEY
echo "$d1" | grep -Eqx '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' ||
  { echo "::error::REGISTRY_D1_DATABASE_ID is not a D1 database ID" >&2; exit 1; }
echo "$client" | grep -Eqx '[A-Za-z0-9._-]+' ||
  { echo "::error::REGISTRY_GITHUB_CLIENT_ID is not a GitHub OAuth client ID" >&2; exit 1; }
[ "${#key}" -ge 32 ] ||
  { echo "::error::REGISTRY_JWT_SIGNING_KEY is shorter than 32 characters" >&2; exit 1; }

dir=$(cd "$(dirname "$0")/.." && pwd)
sed -e "s/REPLACE_WITH_${placeholder}_D1_ID/$d1/" \
  -e "s/REPLACE_WITH_${placeholder}_GITHUB_CLIENT_ID/$client/" \
  "$dir/wrangler.toml" > "$dir/wrangler.toml.new"
mv "$dir/wrangler.toml.new" "$dir/wrangler.toml"
if grep -n "REPLACE_WITH_${placeholder}" "$dir/wrangler.toml" >&2; then
  echo "::error::wrangler.toml has a $1 placeholder this script does not fill" >&2
  exit 1
fi

umask 077
REGISTRY_JWT_SIGNING_KEY=$key node -e \
  'process.stdout.write(JSON.stringify({JWT_SIGNING_KEY: process.env.REGISTRY_JWT_SIGNING_KEY}))' > "$secrets"
