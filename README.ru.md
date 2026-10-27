<p align="center">
  <img src="docs/assets/banner.jpg" alt="TigerSB — VPN-панель" width="100%">
</p>

<p align="center">
  <a href="LICENSE"><img alt="License GPL-3.0" src="https://img.shields.io/badge/license-GPL--3.0-FF6A00?style=for-the-badge&labelColor=1c1c1c"></a>
  <a href="https://github.com/Stilltiger45/TigerSB-panel/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/Stilltiger45/TigerSB-panel/ci.yml?branch=main&style=for-the-badge&label=CI&labelColor=1c1c1c&color=FF6A00"></a>
  <img alt="Debian 11+ и Ubuntu 20.04+" src="https://img.shields.io/badge/Debian%20%7C%20Ubuntu-amd64%20%7C%20arm64-FF6A00?style=for-the-badge&logo=linux&logoColor=white&labelColor=1c1c1c">
</p>

<p align="center">
  <a href="https://github.com/XTLS/Xray-core"><img alt="Xray-core 26.9.30" src="https://img.shields.io/badge/Xray--core-26.9.30-FF8A1F?style=for-the-badge&labelColor=1c1c1c"></a>
  <a href="https://github.com/SagerNet/sing-box"><img alt="sing-box 1.14.4" src="https://img.shields.io/badge/sing--box-1.14.4-FF8A1F?style=for-the-badge&labelColor=1c1c1c"></a>
  <a href="https://caddyserver.com"><img alt="Caddy 2.11.4" src="https://img.shields.io/badge/Caddy-2.11.4-FF8A1F?style=for-the-badge&logo=caddy&logoColor=white&labelColor=1c1c1c"></a>
  <img alt="Docker" src="https://img.shields.io/badge/Docker-2%20cores-FF8A1F?style=for-the-badge&logo=docker&logoColor=white&labelColor=1c1c1c">
</p>

<p align="center">
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-FFB861?style=for-the-badge&logo=go&logoColor=white&labelColor=1c1c1c">
  <img alt="SQLite" src="https://img.shields.io/badge/SQLite-без%20cgo-FFB861?style=for-the-badge&logo=sqlite&logoColor=white&labelColor=1c1c1c">
  <img alt="systemd" src="https://img.shields.io/badge/systemd-службы-FFB861?style=for-the-badge&labelColor=1c1c1c">
</p>

<p align="center">
  🇷🇺 <b>Русский</b> &nbsp;|&nbsp; <a href="README.md">🇬🇧 English</a>
</p>

---

**TigerSB-panel** — своя VPN-панель на Xray и sing-box, которая ставится одной командой. Добавляете человека, отправляете ему ключ или QR-код, и он подключён. Снаружи сервер выглядит как обычный сайт на техобслуживании.

Работает в паре с приложением [TigerSB-client](https://github.com/Stilltiger45/TigerSB-client), а обычные подписки `https://` понимают v2rayNG, Hiddify, Happ, Streisand и другие клиенты на Xray и sing-box.

<p align="center">
  <img src="docs/assets/screens.jpg" alt="Вкладки панели: «Обзор», «Пользователи», «Контроль» и «Протоколы»" width="100%">
</p>

<sub>Скриншоты с тестового сервера.</sub>

## Возможности

- **Установка одной командой.** Установщик ставит панель, Caddy, Xray и sing-box, получает сертификат Let's Encrypt и показывает адрес панели. Повторный запуск ничего не ломает.
- **Панель спрятана.** Она открывается только по секретному пути на вашем домене, а на любой другой адрес сайт показывает безобидную заглушку «Технические работы» (четыре шаблона: выпечка, фотоблог, дача, книги). `tigersb close` прячет и сам секретный путь, а открыть его снова можно только по SSH.
- **Пользователи в два клика.** Имя и число устройств. Каждый получает ключ `tigersb://` и подписку `https://`, обе с QR-кодом. Новый пользователь подключается без перезапуска Xray, остальных не выкидывает.
- **Лимит устройств.** Устройство учитывается по HWID при скачивании подписки, сверх лимита подписка не выдаётся. В списке видно модель, систему, приложение, IP и время последнего обновления, лишнее устройство удаляется кнопкой.
- **Три протокола в двух контейнерах.** Xray держит VLESS + REALITY через XHTTP и gRPC; порт 443 общий у сайта и протоколов, сервер различает подключения по SNI. sing-box держит TUIC v5 (QUIC по UDP 8443) с настоящим сертификатом Let's Encrypt сайта. В карточке протокола виден только порт, остальное свёрнуто под «Все настройки», и любое поле можно поменять. Конфиг проверяется в ядре до применения. Сканер подбирает SNI для REALITY среди соседей сервера. Новый протокол добавляется в то ядро, которое его поддерживает.
- **Защищённые подписки.** Профиль для TigerSB подписан ключом Ed25519, подменить серверы по дороге нельзя. Неверный токен отдаёт ту же заглушку с теми же заголовками, что и любая страница сайта. Подписки работают, даже когда панель закрыта или спит.
- **Контроль сервера.** Графики процессора, памяти, диска и сети за 6 часов, сутки или неделю. Проверка служб, сертификата, домена, места, часов и базы с подсказкой, что делать.
- **Обновление кнопкой.** Перед обновлением создаётся точка восстановления, а если новая версия не запустилась, панель возвращается к ней сама и пишет причину.

Скоро: каскад через точку маршрутизации в России (gRPC и TUIC), XHTTP через Yandex Cloud CDN.

## Компоненты

| Компонент | Роль | Лицензия |
|---|---|---|
| [Xray-core](https://github.com/XTLS/Xray-core) 26.9.30 | VPN-ядро: VLESS + REALITY, XHTTP, gRPC. Официальный образ в Docker, закреплён по хешу | MPL-2.0 |
| [sing-box](https://github.com/SagerNet/sing-box) 1.14.4 | второе VPN-ядро: TUIC v5. Официальный образ в Docker, закреплён по хешу | GPL-3.0-or-later |
| [Caddy](https://caddyserver.com) 2.11.4 | сайт-заглушка, сертификат Let's Encrypt, вход в панель и подписки | Apache-2.0 |
| [Docker](https://www.docker.com) | запуск Xray и sing-box без прав и с файловой системой только для чтения | Apache-2.0 |
| [Go](https://go.dev) 1.26 | панель, служба порта 443, служба подписок, сбор данных | BSD-3-Clause |
| [SQLite](https://sqlite.org) (modernc, без cgo) | пользователи, устройства, настройки, графики | BSD-3-Clause |
| [pkg/profile](https://github.com/Stilltiger45/TigerSB-client/tree/main/pkg/profile) из TigerSB-client | формат и подпись профиля TigerSB | GPL-3.0 |

## Установка

Нужен чистый VPS с Debian 11+ или Ubuntu 20.04+ (amd64 или arm64), домен с A-записью на сервер и доступ root:

```bash
curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/install.sh | sudo bash
```

Установщик спросит:

1. **Роль сервера.** «Основной сервер» выпускает трафик в интернет и держит панель. «Точка маршрутизации» понадобится для каскада.
2. Домен и email для сертификата.
3. Логин и пароль администратора (пароль не короче 10 символов).

В конце он покажет адрес панели вида `https://<домен>/<секретный путь>/`, его нужно сохранить. Забыли адрес — `tigersb info` на сервере. Сборка из исходников занимает пару минут.

Если у хостинга свой файрвол, откройте в нём порты 443 и 8443 для TCP и 8443 для UDP (TUIC).

## Первые шаги

1. Откройте адрес панели и войдите.
2. На вкладке **«Пользователи»** добавьте человека и нажмите на него: появятся ключ TigerSB и подписка https с QR-кодами.
3. Отправьте ключ или покажите QR-код. В [TigerSB](https://github.com/Stilltiger45/TigerSB-client) достаточно вставить ключ, в v2rayNG, Hiddify и других — добавить подписку по ссылке https.
4. Закончили настройку — нажмите «Закрыть панель» на вкладке «Обзор». Открыть снова: `tigersb open` по SSH.

## Обновление и удаление

Обновить проще всего кнопкой **«Обновить панель»** на вкладке «Контроль» или командой `tigersb update`. Настройки, пользователи и база сохраняются, VPN во время обновления работает.

Удалить всё с сервера (спросит подтверждение и предложит сохранить копию настроек и базы):

```bash
curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/uninstall.sh | sudo bash
```

## Команды на сервере

| Команда | Что делает |
|---|---|
| `tigersb info` | показать адрес панели и запасную команду SSH-туннеля |
| `tigersb open` | открыть панель по `https://<домен>/<секретный путь>/` |
| `tigersb close` | спрятать панель: по секретному пути снова заглушка |
| `tigersb site status` | домен, шаблон заглушки, состояние Caddy и сертификата |
| `tigersb site set --domain <домен> [--email <почта>]` | сменить домен из консоли, если панель недоступна |
| `tigersb site apply` | заново записать заглушку и настройки Caddy и применить их |
| `tigersb passwd --admin <логин>` | сменить пароль, все сессии завершаются |
| `tigersb sleep` | усыпить панель: процесс остановлен, по секретному пути заглушка. VPN и подписки продолжают работать |
| `tigersb wake` | разбудить панель |
| `tigersb status` | состояние панели |
| `tigersb update` | обновить панель с GitHub: точка восстановления, обновление, откат при сбое |
| `tigersb restore list` | список точек восстановления |
| `tigersb restore create` | создать точку восстановления |
| `tigersb restore apply <имя>` | вернуть точку восстановления |
| `tigersb xray status` | состояние Xray и порта 443, протоколы и число пользователей |
| `tigersb xray apply` | заново собрать конфиг Xray из базы, проверить и перезапустить |
| `tigersb singbox status` | состояние sing-box и порт TUIC |
| `tigersb singbox apply` | заново собрать конфиг sing-box и копию сертификата, проверить и перезапустить |
| `tigersb monitor` | снять показатели и проверить сервер сейчас (обычно это делает таймер) |

## Документация

- [Руководство по панели](docs/guide.md): каждая вкладка, вход и SSH-туннель, подписки и лимит устройств, обновление и точки восстановления, защита входа.
- [Устройство панели](docs/architecture.md): службы, порты, база, как применяются настройки.
- [Формат профиля и ключа](https://github.com/Stilltiger45/TigerSB-client/blob/main/docs/profile.md) в репозитории клиента.
- [SECURITY.md](SECURITY.md): как сообщить об уязвимости.

## Разработка

```bash
go test ./...
CGO_ENABLED=0 go build -o tigersb ./cmd/tigersb
```

Установка из клона репозитория собирает панель из рабочей копии:

```bash
git clone https://github.com/Stilltiger45/TigerSB-panel.git
cd TigerSB-panel
sudo ./scripts/install.sh            # или --binary ./tigersb-linux-amd64 с готовым бинарником
```

CI проверяет `gofmt`, `go vet`, тесты, скрипты через `shellcheck` и лицензии зависимостей. Тег `v*` собирает релиз с бинарниками `tigersb-linux-amd64` и `tigersb-linux-arm64`.

## Лицензия

GPL-3.0, см. [LICENSE](LICENSE). Лицензия совпадает с лицензией [TigerSB-client](https://github.com/Stilltiger45/TigerSB-client), поэтому панель может подключать его пакет `pkg/profile`. Все зависимости панели распространяются под MIT или BSD и с GPL-3.0 совместимы. Сторонние компоненты, включая Caddy (Apache-2.0), и заимствованные идеи перечислены в [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) и на вкладке «О панели». Это проверяет `scripts/check-licenses.sh` в CI: он падает, если у зависимости нет лицензии или она несовместима (AGPL, SSPL, BSL и подобные).
