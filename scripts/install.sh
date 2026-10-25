#!/usr/bin/env bash
# TigerSB installer: asks for the server role and sets up the panel.
#
#   curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/install.sh | sudo bash
#   sudo ./scripts/install.sh                     # from a repo checkout, builds with Go
#   sudo ./scripts/install.sh --binary ./tigersb  # use a ready binary
#   sudo ./scripts/install.sh --update            # new binary, same config and database
#
# Without a checkout or --binary the latest GitHub release is downloaded; if
# there is no release yet, the main branch is downloaded and built with Go.
set -euo pipefail

REPO="Stilltiger45/TigerSB-panel"
BIN=/usr/local/bin/tigersb
CONF_DIR=/etc/tigersb
DATA_DIR=/var/lib/tigersb
UNIT=/etc/systemd/system/tigersb.service
GO_VERSION=1.26.0

binary=""
role=""
domain=""
email=""
main_server=""
admin=""
update=0

die() { echo "Ошибка: $*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Установка панели TigerSB.

  --binary PATH        готовый бинарник tigersb
  --role main|router   роль сервера (иначе спросит)
  --domain DOMAIN      домен основного сервера
  --email EMAIL        email для сертификата
  --main-server ADDR   адрес основного сервера (для точки маршрутизации)
  --admin LOGIN        логин администратора
  --update             обновить панель, сохранив настройки и базу

Удаление: curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/uninstall.sh | sudo bash

Пароль спрашивается всегда, либо берётся из переменной TIGERSB_PASSWORD.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		--binary) binary="${2:?}"; shift 2 ;;
		--role) role="${2:?}"; shift 2 ;;
		--domain) domain="${2:?}"; shift 2 ;;
		--email) email="${2:?}"; shift 2 ;;
		--main-server) main_server="${2:?}"; shift 2 ;;
		--admin) admin="${2:?}"; shift 2 ;;
		--update) update=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) usage >&2; die "неизвестный параметр $1" ;;
	esac
done

# Prompts read from the terminal so that `curl ... | sudo bash` works too.
ask() {
	local prompt="$1" var
	read -r -p "$prompt" var </dev/tty
	printf '%s' "$var"
}

ask_secret() {
	local prompt="$1" var
	read -r -s -p "$prompt" var </dev/tty
	echo >&2
	printf '%s' "$var"
}

check_system() {
	[ "$(id -u)" -eq 0 ] || die "запустите установщик от root (sudo)"
	[ "$(uname -s)" = Linux ] || die "поддерживается только Linux"
	command -v systemctl >/dev/null || die "нужен systemd"
	case "$(uname -m)" in
		x86_64|amd64) arch=amd64 ;;
		aarch64|arm64) arch=arm64 ;;
		*) die "архитектура $(uname -m) не поддерживается" ;;
	esac
	command -v curl >/dev/null || die "нужен curl: apt install -y curl"
	command -v tar >/dev/null || die "нужен tar: apt install -y tar"
}

show_installed() {
	echo "Панель уже установлена ($CONF_DIR/config.json), состояние: $(systemctl is-active tigersb.service || true)."
	"$BIN" info
	echo
	echo "Обновить:       curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | sudo bash -s -- --update"
	echo "Удалить:        curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/uninstall.sh | sudo bash"
	echo "Сменить пароль: tigersb passwd --admin <логин>"
}

choose_role() {
	if [ -z "$role" ]; then
		echo "Какую роль выполняет этот сервер?"
		echo "  1) Основной сервер (выход в интернет, панель управления)"
		echo "  2) Точка маршрутизации (вход в каскад, пересылает трафик на основной)"
		case "$(ask 'Выберите 1 или 2: ')" in
			1) role=main ;;
			2) role=router ;;
			*) die "нужно выбрать 1 или 2" ;;
		esac
	fi
	case "$role" in
		main)
			[ -n "$domain" ] || domain="$(ask 'Домен сервера (можно оставить пустым): ')"
			if [ -n "$domain" ] && [ -z "$email" ]; then
				email="$(ask 'Email для сертификата Let'"'"'s Encrypt: ')"
			fi
			;;
		router)
			[ -n "$main_server" ] || main_server="$(ask 'Адрес основного сервера (IP или домен): ')"
			[ -n "$main_server" ] || die "для точки маршрутизации нужен адрес основного сервера"
			;;
		*) die "роль должна быть main или router" ;;
	esac
}

choose_admin() {
	[ -n "$admin" ] || admin="$(ask 'Логин администратора панели: ')"
	[ -n "$admin" ] || die "логин не может быть пустым"
	password="${TIGERSB_PASSWORD:-}"
	if [ -z "$password" ]; then
		password="$(ask_secret 'Пароль (не короче 10 символов): ')"
		local again
		again="$(ask_secret 'Повторите пароль: ')"
		[ "$password" = "$again" ] || die "пароли не совпадают"
	fi
	[ "${#password}" -ge 10 ] || die "пароль короче 10 символов"
}

# repo_dir prints the checkout this script runs from, or nothing when piped from curl.
repo_dir() {
	local self="${BASH_SOURCE[0]:-}"
	[ -f "$self" ] || return 0
	local dir
	dir="$(cd "$(dirname "$self")/.." && pwd)"
	if [ -f "$dir/go.mod" ] && [ -f "$dir/cmd/tigersb/main.go" ]; then
		echo "$dir"
	fi
}

# build_from builds the panel from the source tree in $1 into $tmp/tigersb.
build_from() {
	local src="$1" ver="$2" go
	go="$(command -v go || true)"
	[ -n "$go" ] || go="$(install_go "$tmp")"
	echo "Собираю tigersb ($ver)..."
	(cd "$src" && CGO_ENABLED=0 GOFLAGS=-trimpath "$go" build -ldflags "-s -w -X main.version=$ver" -o "$tmp/tigersb" ./cmd/tigersb)
}

install_go() {
	local dir="$1"
	echo "Скачиваю Go $GO_VERSION для сборки..." >&2
	curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -xz -C "$dir"
	echo "$dir/go/bin/go"
}

get_binary() {
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	if [ -n "$binary" ]; then
		[ -f "$binary" ] || die "файл $binary не найден"
		install -m 0755 "$binary" "$BIN"
		return
	fi
	local src
	src="$(repo_dir)"
	if [ -n "$src" ]; then
		build_from "$src" "$(git -C "$src" describe --tags --always --dirty 2>/dev/null || echo dev)"
	elif curl -fsSL -o "$tmp/tigersb" "https://github.com/$REPO/releases/latest/download/tigersb-linux-$arch" 2>/dev/null; then
		echo "Скачан последний релиз $REPO."
	else
		echo "Релизов пока нет, скачиваю исходники из ветки main..."
		mkdir -p "$tmp/src"
		curl -fsSL "https://github.com/$REPO/archive/refs/heads/main.tar.gz" | tar -xz -C "$tmp/src" --strip-components=1
		build_from "$tmp/src" main
	fi
	install -m 0755 "$tmp/tigersb" "$BIN"
}

install_unit() {
	local src
	src="$(repo_dir)"
	if [ -n "$src" ] && [ -f "$src/deploy/tigersb.service" ]; then
		install -m 0644 "$src/deploy/tigersb.service" "$UNIT"
	else
		curl -fsSL -o "$UNIT" "https://raw.githubusercontent.com/$REPO/main/deploy/tigersb.service"
		chmod 0644 "$UNIT"
	fi
	systemctl daemon-reload
}

# restore sets the panel up from a config and database unpacked from a backup.
restore() {
	echo "Найдены настройки из резервной копии ($CONF_DIR/config.json), ставлю панель с ними."
	get_binary
	install_unit
	systemctl enable --now tigersb.service
	"$BIN" info
}

do_update() {
	[ -e "$CONF_DIR/config.json" ] || die "панель не установлена, запустите установщик без --update"
	local was
	was="$(systemctl is-active tigersb.service 2>/dev/null || true)"
	get_binary
	install_unit
	# A sleeping panel stays asleep after the update.
	if [ "$was" = active ]; then
		systemctl restart tigersb.service
	fi
	echo "Панель обновлена до $("$BIN" version), состояние: $(systemctl is-active tigersb.service || true)."
}

main() {
	check_system
	if [ "$update" -eq 1 ]; then
		do_update
		return
	fi
	if [ -e "$CONF_DIR/config.json" ]; then
		if [ -x "$BIN" ]; then
			show_installed
		else
			restore
		fi
		return
	fi
	choose_role
	choose_admin
	get_binary
	install -d -m 0700 "$CONF_DIR" "$DATA_DIR"

	local args=(init --config "$CONF_DIR/config.json" --db "$DATA_DIR/panel.db" --role "$role" --admin "$admin" --password-stdin)
	[ -z "$domain" ] || args+=(--domain "$domain")
	[ -z "$email" ] || args+=(--email "$email")
	[ -z "$main_server" ] || args+=(--main-server "$main_server")
	printf '%s\n' "$password" | "$BIN" "${args[@]}"
	unset password

	install_unit
	systemctl enable --now tigersb.service
	echo
	echo "Готово. Состояние панели: $(systemctl is-active tigersb.service || true). Повторно показать адрес: tigersb info"
	echo "Усыпить панель: tigersb sleep. Разбудить: tigersb wake"
	echo "Удалить полностью: curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/uninstall.sh | sudo bash"
}

main
