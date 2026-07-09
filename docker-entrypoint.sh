#!/bin/sh
set -eu

app_uid=10001
app_gid=10001
data_dir=/data

if [ "$#" -eq 0 ] || [ "${1#-}" != "$1" ]; then
	set -- gitops-dashboard "$@"
fi

is_truthy() {
	case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
		1 | true | yes | y | on)
			return 0
			;;
		*)
			return 1
			;;
	esac
}

data_needs_chown() {
	[ -d "$data_dir" ] || return 1
	[ "$(stat -c '%u:%g' "$data_dir")" = "$app_uid:$app_gid" ] || return 0
	for path in \
		"$data_dir/gitops-dashboard.db" \
		"$data_dir/gitops-dashboard.db-shm" \
		"$data_dir/gitops-dashboard.db-wal" \
		"$data_dir/repos"
	do
		[ -e "$path" ] || continue
		[ "$(stat -c '%u:%g' "$path")" = "$app_uid:$app_gid" ] || return 0
	done
	return 1
}

drop_privileges() {
	extra_groups=
	for group in $(printf '%s %s\n' "${GITOPS_DASHBOARD_SUPPLEMENTAL_GROUPS:-}" "${DOCKER_SOCKET_GID:-}" | tr ',' ' '); do
		case "$group" in
			"" | 0 | "$app_gid")
				continue
				;;
			*[!0-9]*)
				echo "gitops-dashboard entrypoint: ignoring non-numeric supplemental group $group" >&2
				continue
				;;
		esac
		case ",$extra_groups," in
			*,"$group",*)
				continue
				;;
		esac
		if [ -n "$extra_groups" ]; then
			extra_groups="$extra_groups,$group"
		else
			extra_groups="$group"
		fi
	done

	if [ -n "$extra_groups" ]; then
		exec setpriv --reuid="$app_uid" --regid="$app_gid" --groups "$extra_groups" "$@"
	fi
	exec setpriv --reuid="$app_uid" --regid="$app_gid" --clear-groups "$@"
}

if [ "$(id -u)" = 0 ]; then
	if is_truthy "${GITOPS_DASHBOARD_SKIP_DATA_CHOWN:-}"; then
		echo "gitops-dashboard entrypoint: skipping /data ownership repair because GITOPS_DASHBOARD_SKIP_DATA_CHOWN is set" >&2
	elif data_needs_chown; then
		echo "gitops-dashboard entrypoint: repairing /data ownership for uid $app_uid gid $app_gid" >&2
		chown -R "$app_uid:$app_gid" "$data_dir"
	fi
	drop_privileges "$@"
fi

exec "$@"
