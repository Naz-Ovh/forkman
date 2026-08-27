#!/bin/sh
# Fail if go.mod gains a direct dependency outside the allowlist.
set -eu

ALLOWED="charm.land/bubbletea/v2
charm.land/bubbles/v2
charm.land/lipgloss/v2"

main_module=$(go list -m)
status=0

direct=$(go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all)

for path in $direct; do
	[ -z "$path" ] && continue
	[ "$path" = "$main_module" ] && continue
	if ! printf '%s\n' "$ALLOWED" | grep -qxF "$path"; then
		echo "forbidden direct dependency: $path" >&2
		status=1
	fi
done

for want in $ALLOWED; do
	if ! printf '%s\n' "$direct" | grep -qxF "$want"; then
		echo "expected direct dependency missing: $want" >&2
		status=1
	fi
done

if [ "$status" -eq 0 ]; then
	echo "dependency budget ok: exactly 3 direct dependencies"
fi
exit "$status"
