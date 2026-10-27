#!/usr/bin/env bash
# TigerSB uninstaller: removes the panel, its services, config and database.
#
#   curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/uninstall.sh | sudo bash
#   sudo ./scripts/uninstall.sh --yes             # without questions, no backup
set -euo pipefail

BIN=/usr/local/bin/tigersb
CONF_DIR=/etc/tigersb
DATA_DIR=/var/lib/tigersb
LIB_DIR=/usr/local/lib/tigersb
CACHE_DIR=/var/cache/tigersb
UNIT_DIR=/etc/systemd/system

yes=0
case "${1:-}" in
	--yes|-y) yes=1 ;;
	"") ;;
	*) echo "Использование: uninstall.sh [--yes]" >&2; exit 2 ;;
esac

die() { echo "Ошибка: $*" >&2; exit 1; }

# Prompts read from the terminal so that `curl ... | sudo bash` works too.
ask() {
	local var
	read -r -p "$1" var </dev/tty
	printf '%s' "$var"
}

[ "$(id -u)" -eq 0 ] || die "запустите от root (sudo)"

# Every unit the panel creates is named tigersb*.service or tigersb*.timer, so
# later stages (Xray, sing-box) are removed by the same pattern. Timers go
# first, so they cannot start a service that is being removed.
shopt -s nullglob
units=()
for f in "$UNIT_DIR"/tigersb-*.timer "$UNIT_DIR"/tigersb.service "$UNIT_DIR"/tigersb-*.service "$UNIT_DIR"/tigersb@.service; do
	[ -e "$f" ] || continue
	units+=("$(basename "$f")")
done
shopt -u nullglob

if [ ! -e "$BIN" ] && [ ! -d "$LIB_DIR" ] && [ ! -d "$CONF_DIR" ] && [ ! -d "$DATA_DIR" ] && [ ${#units[@]} -eq 0 ]; then
	echo "TigerSB не установлен, удалять нечего."
	exit 0
fi

echo "Будет удалено:"
for u in "${units[@]}"; do
	echo "  - $u"
done
[ ! -e "$BIN" ] || echo "  - $BIN"
[ ! -d "$LIB_DIR" ] || echo "  - $LIB_DIR (Caddy)"
[ ! -d "$CONF_DIR" ] || echo "  - $CONF_DIR (настройки и секретный путь)"
[ ! -d "$DATA_DIR" ] || echo "  - $DATA_DIR (база, пользователи VPN, сертификаты, заглушка, точки восстановления)"
if command -v docker >/dev/null; then
	echo "  - контейнеры tigersb-xray и tigersb-singbox, образы Xray и sing-box (сам Docker останется)"
fi
echo

backup=0
if [ "$yes" -eq 0 ]; then
	case "$(ask 'Удалить TigerSB полностью? Это необратимо [y/N]: ')" in
		y|Y|д|Д) ;;
		*) echo "Отменено."; exit 0 ;;
	esac
	if [ -d "$DATA_DIR" ] || [ -d "$CONF_DIR" ]; then
		case "$(ask 'Сохранить копию настроек и базы для переноса на другой сервер? [y/N]: ')" in
			y|Y|д|Д) backup=1 ;;
		esac
	fi
fi

if [ "$backup" -eq 1 ]; then
	archive="/root/tigersb-backup-$(date +%Y%m%d-%H%M%S).tar.gz"
	dirs=()
	for d in "$CONF_DIR" "$DATA_DIR"; do
		[ ! -d "$d" ] || dirs+=("${d#/}")
	done
	# Stop first so the SQLite database is not copied mid-write.
	systemctl stop tigersb-monitor.timer tigersb.service tigersb-caddy.service tigersb-edge.service tigersb-sub.service tigersb-xray.service tigersb-singbox.service 2>/dev/null || true
	# Restore points are only useful on this server; they would bloat the copy.
	(umask 077 && tar -czf "$archive" --exclude="${DATA_DIR#/}/restore" -C / "${dirs[@]}")
	echo "Копия сохранена: $archive"
	echo "Перенос: скопируйте архив на новый сервер, выполните там tar -xzf <архив> -C / и запустите установщик."
fi

for u in "${units[@]}"; do
	echo "Останавливаю $u..."
	systemctl disable --now "$u" 2>/dev/null || true
	systemctl reset-failed "$u" 2>/dev/null || true
	rm -f "${UNIT_DIR:?}/$u"
done
systemctl daemon-reload 2>/dev/null || true

if command -v docker >/dev/null; then
	docker rm -f tigersb-xray tigersb-singbox >/dev/null 2>&1 || true
	for repo in ghcr.io/xtls/xray-core ghcr.io/sagernet/sing-box; do
		images="$(docker images -q "$repo" 2>/dev/null || true)"
		if [ -n "$images" ]; then
			echo "Удаляю образ $repo..."
			# shellcheck disable=SC2086 # one id per word
			docker rmi -f $images >/dev/null 2>&1 || true
		fi
	done
fi

echo "Удаляю файлы..."
rm -rf "$CONF_DIR" "$DATA_DIR" "$BIN" "$LIB_DIR" "$CACHE_DIR"
# The BBR permission stays in effect until reboot; the module stays loaded.
rm -f /etc/sysctl.d/90-tigersb-bbr.conf /etc/modules-load.d/tigersb-bbr.conf

echo
echo "TigerSB полностью удалён."
