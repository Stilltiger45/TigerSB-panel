#!/usr/bin/env bash
# TigerSB installer: asks for the server role and sets up the panel.
#
#   curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/install.sh | sudo bash
#   sudo ./scripts/install.sh                     # from a repo checkout, builds with Go
#   sudo ./scripts/install.sh --binary ./tigersb  # use a ready binary
#   sudo ./scripts/install.sh --update            # new binary, same config and database
#   sudo ./scripts/install.sh --update --ref SHA  # build that commit (the panel's update button)
#
# Without a checkout or --binary the latest GitHub release is downloaded; if
# there is no release yet, the main branch is downloaded and built with Go.
set -euo pipefail

REPO="Stilltiger45/TigerSB-panel"
BIN=/usr/local/bin/tigersb
CONF_DIR=/etc/tigersb
DATA_DIR=/var/lib/tigersb
GO_VERSION=1.26.0

# Caddy serves the maintenance page and gets the certificate. Pinned release
# with checksums from caddy_2.11.4_checksums.txt (Apache-2.0, unmodified).
LIB_DIR=/usr/local/lib/tigersb
# Go downloads modules and keeps build results here while it builds; about
# 400 MB, removed when the installer exits. Set explicitly because the
# panel's update button runs the installer as a systemd service, where HOME
# is not set and Go has no default place for them. Not in /tmp, which is in
# memory on some systems.
CACHE_DIR=/var/cache/tigersb
CADDY_VERSION=2.11.4
caddy_sha512() {
	case "$1" in
		amd64) echo 8220d1f013b6f27510247b2360c9e0ca9f018feebd82515f07635318b34ff9777ccc8fd0b6e6f2486ce3a33fe389fbb7db12d05baa474f4587509fb4f5ebf1c9 ;;
		arm64) echo d5a7c423853c24a799765e0e8210d5c7c22a8f56ed37a3cae2fb9f58be138853c02b4efd6b59d576e6d8c7c0d30b9c1592deeaa6a536ff69bcca23b8c1ea709c ;;
	esac
}

binary=""
role=""
domain=""
email=""
main_server=""
admin=""
update=0
ref=""

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
  --ref SHA            собрать этот коммит ветки main (так обновляет кнопка в панели)

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
		--ref) ref="${2:?}"; [[ "$ref" =~ ^[0-9a-f]{40}$ ]] || die "--ref ждёт полный хеш коммита"; shift 2 ;;
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
	mkdir -p "$CACHE_DIR"
	(cd "$src" && HOME="${HOME:-/root}" GOPATH="$CACHE_DIR/go" GOMODCACHE="$CACHE_DIR/go/pkg/mod" GOCACHE="$CACHE_DIR/go-build" \
		CGO_ENABLED=0 GOFLAGS=-trimpath "$go" build -ldflags "-s -w -X main.version=$ver" -o "$tmp/tigersb" ./cmd/tigersb)
}

install_go() {
	local dir="$1"
	echo "Скачиваю Go $GO_VERSION для сборки..." >&2
	curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -xz -C "$dir"
	echo "$dir/go/bin/go"
}

get_binary() {
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp" "$CACHE_DIR"' EXIT
	if [ -n "$binary" ]; then
		[ -f "$binary" ] || die "файл $binary не найден"
		install -m 0755 "$binary" "$BIN"
		return
	fi
	local src
	src="$(repo_dir)"
	if [ -n "$src" ] && [ -z "$ref" ]; then
		build_from "$src" "$(git -C "$src" describe --tags --always --dirty 2>/dev/null || echo dev)"
	elif [ -z "$ref" ] && curl -fsSL -o "$tmp/tigersb" "https://github.com/$REPO/releases/latest/download/tigersb-linux-$arch" 2>/dev/null; then
		echo "Скачан последний релиз $REPO."
	else
		# The panel names such builds main-<short sha> and compares that with
		# GitHub to tell whether an update is available.
		[ -n "$ref" ] || ref="$(main_sha)"
		mkdir -p "$tmp/src"
		if [ -n "$ref" ]; then
			echo "Скачиваю исходники коммита ${ref:0:7} из ветки main..."
			curl -fsSL "https://github.com/$REPO/archive/$ref.tar.gz" | tar -xz -C "$tmp/src" --strip-components=1
			build_from "$tmp/src" "main-${ref:0:7}"
		else
			echo "Релизов пока нет, скачиваю исходники из ветки main..."
			curl -fsSL "https://github.com/$REPO/archive/refs/heads/main.tar.gz" | tar -xz -C "$tmp/src" --strip-components=1
			build_from "$tmp/src" main
		fi
	fi
	install -m 0755 "$tmp/tigersb" "$BIN"
}

# main_sha prints the newest commit of main, or nothing when GitHub's API is unreachable.
main_sha() {
	local sha
	sha="$(curl -fsSL -H 'Accept: application/vnd.github.sha' "https://api.github.com/repos/$REPO/commits/main" 2>/dev/null || true)"
	if [[ "$sha" =~ ^[0-9a-f]{40}$ ]]; then
		echo "$sha"
	fi
}

UNITS="tigersb.service tigersb-caddy.service tigersb-edge.service tigersb-sub.service tigersb-xray.service tigersb-singbox.service tigersb-monitor.service tigersb-monitor.timer tigersb-update.service"

install_unit() {
	local src name
	# Units come from the same source tree the binary was built from.
	src="$(repo_dir)"
	if [ -d "${tmp:-}/src/deploy" ]; then
		src="$tmp/src"
	fi
	for name in $UNITS; do
		if [ -n "$src" ] && [ -f "$src/deploy/$name" ]; then
			install -m 0644 "$src/deploy/$name" "/etc/systemd/system/$name"
		else
			curl -fsSL -o "/etc/systemd/system/$name" "https://raw.githubusercontent.com/$REPO/${ref:-main}/deploy/$name"
			chmod 0644 "/etc/systemd/system/$name"
		fi
	done
	systemctl daemon-reload
}

# start_monitor turns on the 5-minute resource and health check.
start_monitor() {
	systemctl enable --now tigersb-monitor.timer
}

# check_ports stops a fresh install when another web server holds 80 or 443,
# and warns when the default gRPC (TCP) or TUIC (UDP) port is taken.
check_ports() {
	command -v ss >/dev/null || return 0
	local port who
	for port in 80 443; do
		who="$(ss -ltnpH "sport = :$port" 2>/dev/null | grep -v -e '"caddy"' -e '"tigersb"' | head -n1 || true)"
		[ -z "$who" ] || die "порт $port занят ($who). Остановите этот веб-сервер (например, systemctl disable --now nginx) и запустите установщик снова"
	done
	who="$(ss -ltnpH "sport = :8443" 2>/dev/null | grep -v '"xray"' | head -n1 || true)"
	[ -z "$who" ] || echo "Внимание: порт 8443 занят ($who). Поменяйте порт gRPC на вкладке «Протоколы»." >&2
	who="$(ss -lunpH "sport = :8443" 2>/dev/null | grep -v '"sing-box"' | head -n1 || true)"
	[ -z "$who" ] || echo "Внимание: порт 8443/udp занят ($who). Поменяйте порт TUIC на вкладке «Протоколы»." >&2
}

# install_docker makes sure Docker runs: Xray and sing-box live in containers. It prints
# nothing and returns 1 when Docker cannot be installed here.
install_docker() {
	if ! command -v docker >/dev/null; then
		if ! command -v apt-get >/dev/null; then
			echo "Docker не найден, а apt-get нет: установите Docker вручную и запустите установщик с --update." >&2
			return 1
		fi
		echo "Устанавливаю Docker (пакет docker.io)..." >&2
		DEBIAN_FRONTEND=noninteractive apt-get update -qq >&2 || true
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io >&2 || {
			echo "Docker не установился: VPN не запустится, пока его нет." >&2
			return 1
		}
	fi
	systemctl enable --now docker.service >&2 || return 1
}

# start_vpn starts the edge router on 443, the subscriptions, and Xray and
# sing-box in Docker. Run it after start_site: with a domain Caddy moves to
# 127.0.0.1:8444, frees 443 and already reads the edge's PROXY header.
start_vpn() {
	install -d -m 0700 "$DATA_DIR/edge" "$DATA_DIR/docker" "$DATA_DIR/singbox"
	systemctl enable tigersb-edge.service tigersb-sub.service tigersb-xray.service tigersb-singbox.service
	systemctl restart tigersb-edge.service tigersb-sub.service
	if install_docker; then
		local image core
		for core in xray singbox; do
			image="$("$BIN" "$core" image)"
			echo "Скачиваю образ $core ($image)..."
			docker pull -q "$image" >/dev/null || echo "Образ не скачался, $core попробует ещё раз при запуске." >&2
		done
		systemctl restart tigersb-xray.service tigersb-singbox.service
	fi
}

# install_caddy puts the pinned Caddy release into $LIB_DIR. It prints
# "changed" when a new binary was installed.
install_caddy() {
	if [ -x "$LIB_DIR/caddy" ] && "$LIB_DIR/caddy" version 2>/dev/null | grep -q "^v$CADDY_VERSION "; then
		return
	fi
	local dir
	dir="$(mktemp -d)"
	echo "Скачиваю Caddy $CADDY_VERSION..." >&2
	curl -fsSL -o "$dir/caddy.tar.gz" \
		"https://github.com/caddyserver/caddy/releases/download/v$CADDY_VERSION/caddy_${CADDY_VERSION}_linux_$arch.tar.gz"
	echo "$(caddy_sha512 "$arch")  $dir/caddy.tar.gz" | sha512sum -c --quiet - >&2 || die "контрольная сумма Caddy не совпала"
	tar -xzf "$dir/caddy.tar.gz" -C "$dir" caddy LICENSE
	install -d -m 0755 "$LIB_DIR"
	install -m 0755 "$dir/caddy" "$LIB_DIR/caddy"
	install -m 0644 "$dir/LICENSE" "$LIB_DIR/caddy.LICENSE"
	rm -rf "$dir"
	echo changed
}

# start_site writes the maintenance page and Caddyfile, then starts Caddy.
# Start the panel first: the route to it is published only while it runs.
start_site() {
	install -d -m 0700 "$DATA_DIR/caddy"
	install -d -m 0755 "$DATA_DIR/www"
	"$BIN" site apply --config "$CONF_DIR/config.json" >/dev/null
	systemctl enable --now tigersb-caddy.service
}

# restore sets the panel up from a config and database unpacked from a backup.
restore() {
	echo "Найдены настройки из резервной копии ($CONF_DIR/config.json), ставлю панель с ними."
	check_ports
	get_binary
	install_caddy >/dev/null
	install_unit
	systemctl enable --now tigersb.service
	start_site
	start_vpn
	start_monitor
	"$BIN" info
}

do_update() {
	[ -e "$CONF_DIR/config.json" ] || die "панель не установлена, запустите установщик без --update"
	local was
	was="$(systemctl is-active tigersb.service 2>/dev/null || true)"
	get_binary
	local caddy_changed
	caddy_changed="$(install_caddy)"
	install_unit
	# A sleeping panel stays asleep after the update.
	if [ "$was" = active ]; then
		systemctl restart tigersb.service
	fi
	# Rewrite the Caddyfile with the new binary, so new routes take effect.
	start_site
	if [ -n "$caddy_changed" ]; then
		systemctl restart tigersb-caddy.service
	fi
	start_vpn
	start_monitor
	echo "Панель обновлена до $("$BIN" version), состояние: $(systemctl is-active tigersb.service || true)."
	"$BIN" info
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
	check_ports
	choose_role
	choose_admin
	get_binary
	install_caddy >/dev/null
	install -d -m 0700 "$CONF_DIR" "$DATA_DIR"

	local args=(init --config "$CONF_DIR/config.json" --db "$DATA_DIR/panel.db" --role "$role" --admin "$admin" --password-stdin)
	[ -z "$domain" ] || args+=(--domain "$domain")
	[ -z "$email" ] || args+=(--email "$email")
	[ -z "$main_server" ] || args+=(--main-server "$main_server")
	printf '%s\n' "$password" | "$BIN" "${args[@]}"
	unset password

	install_unit
	systemctl enable --now tigersb.service
	start_site
	start_vpn
	start_monitor
	echo
	"$BIN" site status --config "$CONF_DIR/config.json" || true
	echo
	"$BIN" info --config "$CONF_DIR/config.json"
	echo
	echo "Готово. Повторно показать адрес: tigersb info"
	echo "Открыть панель на домене: tigersb open. Спрятать за заглушку: tigersb close"
	echo "Усыпить панель: tigersb sleep. Разбудить: tigersb wake"
	echo "Удалить полностью: curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/uninstall.sh | sudo bash"
}

main
