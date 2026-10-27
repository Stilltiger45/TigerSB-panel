#!/usr/bin/env bash
# Fails when a dependency has no license file or carries a copyleft license
# that the GPL-3.0 panel cannot combine with. Run from the repository root.
set -euo pipefail

bad=0
while read -r dir path; do
	[ -n "$dir" ] || continue
	file=$(find "$dir" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' \) | head -n 1)
	if [ -z "$file" ]; then
		echo "НЕТ ЛИЦЕНЗИИ: $path" >&2
		bad=1
		continue
	fi
	# Only the title counts: the GPL-3.0 text itself mentions the AGPL.
	if head -n 20 "$file" | grep -qiE 'GNU AFFERO|SERVER SIDE PUBLIC LICENSE|Business Source License' || grep -qi 'Commons Clause' "$file"; then
		echo "НЕСОВМЕСТИМАЯ ЛИЦЕНЗИЯ: $path ($file)" >&2
		bad=1
		continue
	fi
	first=$(grep -m1 -iE 'MIT License|Apache License|BSD|Redistribution and use|Permission is hereby granted|Mozilla Public|GNU (Lesser )?General Public|ISC' "$file" || true)
	[ -n "$first" ] || { echo "ЛИЦЕНЗИЯ НЕ ОПОЗНАНА: $path ($file)" >&2; bad=1; continue; }
	printf '%-55s %s\n' "$path" "$(echo "$first" | cut -c1-50)"
done < <(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Dir}} {{.Path}}{{end}}{{end}}' ./cmd/tigersb | sort -u)
exit "$bad"
