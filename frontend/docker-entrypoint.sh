#!/bin/sh
set -eu

# Default backend upstream inside docker-compose network.
# You can set it as either:
#   - backend:5000
#   - http://backend:5000
# We normalize it to a full URL for nginx `proxy_pass`.
: "${BACKEND_UPSTREAM:=backend:5000}"

# Trim whitespace (cheap) and trailing slashes
BACKEND_UPSTREAM="$(printf "%s" "$BACKEND_UPSTREAM" | tr -d ' ' | sed 's:/*$::')"

# If user passed host:port, prefix http://
case "$BACKEND_UPSTREAM" in
  http://*|https://*) : ;;
  *) BACKEND_UPSTREAM="http://$BACKEND_UPSTREAM" ;;
esac

export BACKEND_UPSTREAM
envsubst '${BACKEND_UPSTREAM}' < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf

exec nginx -g 'daemon off;'

