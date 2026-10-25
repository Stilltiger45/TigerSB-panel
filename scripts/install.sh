#!/usr/bin/env bash
# TigerSB installer: asks for the server role and sets up the panel.
#
#   sudo ./scripts/install.sh                     # from a repo checkout, builds with Go
#   sudo ./scripts/install.sh --binary ./tigersb  # use a ready binary
#
# Without a checkout or --binary the latest GitHub release is downloaded.
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
	[ ! -e "$CONF_DIR/config.json" ] || die "панель уже установлена ($CONF_DIR/config.json). Сменить пароль: tigersb passwd --admin <логин>"
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
	local dir
	dir="$(dirname "${BASH_SOURCE[0]:-$0}")/.."
	if [ -d "$dir" ]; then
		(cd "$dir" && pwd)
	fi
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
	if [ -n "$src" ] && [ -f "$src/go.mod" ] && [ -f "$src/cmd/tigersb/main.go" ]; then
		local go
		go="$(command -v go || true)"
		[ -n "$go" ] || go="$(install_go "$tmp")"
		echo "Собираю tigersb из $src..."
		local ver
		ver="$(git -C "$src" describe --tags --always --dirty 2>/dev/null || echo dev)"
		(cd "$src" && CGO_ENABLED=0 GOFLAGS=-trimpath "$go" build -ldflags "-s -w -X main.version=$ver" -o "$tmp/tigersb" ./cmd/tigersb)
	else
		echo "Скачиваю последний релиз $REPO..."
		curl -fsSL -o "$tmp/tigersb" "https://github.com/$REPO/releases/latest/download/tigersb-linux-$arch" \
			|| die "не удалось скачать релиз; запустите установщик из клона репозитория или передайте --binary"
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
	systemctl enable --now tigersb.service
}

main() {
	check_system
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
	echo
	echo "Готово. Панель: $(tigersb status). Повторно показать адрес: tigersb info"
	echo "Усыпить панель: tigersb sleep. Разбудить: tigersb wake"
}

main
