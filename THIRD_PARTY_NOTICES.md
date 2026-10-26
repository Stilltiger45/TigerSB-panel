# Сторонние компоненты и лицензии

TigerSB-panel распространяется под [GPL-3.0](LICENSE). Ниже всё стороннее, что попадает на сервер вместе с панелью, и чужие идеи, которые мы использовали.

## Встроено в бинарник `tigersb`

Список совпадает с выводом `scripts/check-licenses.sh`, который проверяет совместимость лицензий в CI.

| Модуль | Лицензия | Зачем |
|---|---|---|
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | BSD-3-Clause | база данных панели без cgo |
| [modernc.org/libc](https://gitlab.com/cznic/libc), [mathutil](https://gitlab.com/cznic/mathutil), [memory](https://gitlab.com/cznic/memory) | BSD-3-Clause | зависимости SQLite |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) | BSD-3-Clause | Argon2id для паролей |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys), [golang.org/x/term](https://pkg.go.dev/golang.org/x/term) | BSD-3-Clause | ввод пароля в терминале |
| [github.com/google/uuid](https://github.com/google/uuid) | BSD-3-Clause | зависимость SQLite |
| [github.com/remyoudompheng/bigfft](https://github.com/remyoudompheng/bigfft) | BSD-3-Clause | зависимость SQLite |
| [github.com/dustin/go-humanize](https://github.com/dustin/go-humanize) | MIT | зависимость SQLite |
| [rsc.io/qr](https://github.com/rsc/qr) | BSD-3-Clause | QR-коды ссылок пользователей |

Тексты лицензий лежат в исходниках модулей (`go mod download`, затем `$(go env GOMODCACHE)`).

## Устанавливается рядом с панелью

| Программа | Лицензия | Как используется |
|---|---|---|
| [Caddy](https://github.com/caddyserver/caddy) 2.11.4 | Apache-2.0 | Веб-сервер заглушки, выпуск и продление сертификатов Let's Encrypt. Установщик скачивает официальный релиз с GitHub, сверяет SHA-512 и ставит его без изменений в `/usr/local/lib/tigersb/caddy`. Текст лицензии кладётся рядом: `/usr/local/lib/tigersb/caddy.LICENSE`. |
| [Xray-core](https://github.com/XTLS/Xray-core) 26.9.30 | MPL-2.0 | VPN-ядро: VLESS + REALITY через gRPC и XHTTP. Установщик скачивает официальный Docker-образ `ghcr.io/xtls/xray-core`, закреплённый по хешу, и запускает его без изменений как отдельную программу. Исходники и текст лицензии: репозиторий XTLS/Xray-core. |
| [Docker](https://www.docker.com/) (`docker.io` из пакетов дистрибутива) | Apache-2.0 | Запуск контейнера Xray. |

## Заимствованные идеи и материалы

| Источник | Лицензия | Что взято |
|---|---|---|
| [web-panel-proxy](https://github.com/POLESNIESOVETI12/web-panel-proxy) | MIT | Подход к сертификату: только TLS-ALPN на порту 443 (`disable_http_challenge`, `auto_https disable_redirects`), порт 80 закрыт; установка закреплённой версии Caddy с проверкой SHA-512. Схема обновления из панели: отдельная служба вне cgroup панели, временная точка отката, статус и журнал в файлах, понятная причина сбоя. Учёт steal отдельно от загрузки процессора. Код не копировался, всё написано заново на Go. |
| [OlConnect_manager](https://github.com/Oleglog/OlConnect_manager) | WTFPL | Схема установки и полного удаления одной командой `curl … \| sudo bash`. Код не копировался. |
| [RealiTLScanner](https://github.com/XTLS/RealiTLScanner) | MPL-2.0 | Идея сканера SNI: TLS-рукопожатие без SNI с соседними адресами и проверка TLS 1.3, HTTP/2 и X25519. Код не копировался, сканер написан заново на Go. |
| [TigerSB-client](https://github.com/Stilltiger45/TigerSB-client) | GPL-3.0 | Логотип `web/assets/static/tiger.svg` и палитра. Тот же автор. |

Шаблоны заглушки (`internal/site/stubs/`) нарисованы для TigerSB и распространяются под GPL-3.0 вместе с панелью.
