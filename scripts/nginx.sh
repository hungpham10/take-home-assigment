
#!/bin/sh
set -e

echo "============================================="
echo "Nginx starting now..."
echo "============================================="

exec /usr/local/openresty/nginx/sbin/nginx -g "daemon off;"
