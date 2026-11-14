#!/bin/sh
# Fail if Kconfig dropped or changed an explicitly required setting.
# Check both positive assignments and disabled symbols: a monolithic
# kernel must never silently gain a dependency on loadable modules.
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 required-fragment resolved-config" >&2
  exit 2
fi

fragment=$1
config=$2
if [ ! -r "$fragment" ] || [ ! -r "$config" ]; then
  echo "kernel fragment and resolved config must both be readable" >&2
  exit 2
fi

required=$(grep -E '^(CONFIG_[A-Z0-9_]+=|# CONFIG_[A-Z0-9_]+ is not set$)' "$fragment" || true)
if [ -z "$required" ]; then
  echo "kernel fragment contains no required settings" >&2
  exit 2
fi

failed=0
while IFS= read -r setting; do
  if ! grep -Fqx -- "$setting" "$config"; then
    echo "MISSING $setting in resolved kernel config" >&2
    failed=1
  fi
done <<EOF
$required
EOF

exit "$failed"
