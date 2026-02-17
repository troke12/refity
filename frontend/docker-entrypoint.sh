#!/bin/sh
set -eu

# Default backend upstream inside docker-compose network.
# Override this env var if your backend service name differs.
: "${BACKEND_UPSTREAM:=backend:5000}"

envsubst '${BACKEND_UPSTREAM}' < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf

exec nginx -g 'daemon off;'

