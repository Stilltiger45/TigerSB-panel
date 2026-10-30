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

**TigerSB-panel** — свой VPN на своём сервере. Вы арендуете VPS, запускаете одну команду, и через пару минут у вас есть веб-панель: добавляете человека, отправляете ему ключ или QR-код, и он подключён. Снаружи сервер выглядит как обычный сайт на техобслуживании.

Работает в паре с приложением [TigerSB](https://github.com/Stilltiger45/TigerSB-client), а обычные подписки `https://` понимают v2rayNG, Hiddify, Happ, Streisand и другие клиенты на Xray и sing-box.

<p align="center">
  <img src="docs/assets/screens.jpg" alt="Вкладки панели: «Обзор», «Пользователи», «Контроль» и «Протоколы»" width="100%">
</p>

<sub>Скриншоты с тестового сервера.</sub>

**Оглавление:** [Для пользователей](#для-пользователей) · [Для разработчиков](#для-разработчиков)

# Для пользователей

## Что умеет

- **Ставится одной командой** и сама получает сертификат для домена. Повторный запуск ничего не ломает.
- **Прячется.** Панель открывается только по секретному адресу, на всех остальных адресах сайт показывает безобидную заглушку «Технические работы». Кнопка «Закрыть панель» прячет и секретный адрес, открыть его снова можно только по SSH.
- **Пользователь в два клика:** имя и число устройств. Каждый получает ключ `tigersb://` и подписку `https://`, обе с QR-кодом.
- **Лимит устройств.** Лишние телефоны и компьютеры подписку не получат, а любое устройство можно отключить кнопкой.
- **Шесть протоколов:** VLESS + REALITY через XHTTP и gRPC, TUIC, Hysteria 2, NaïveProxy и Mieru. Всё настроено сразу, менять ничего не обязательно. Для каждого можно сделать отдельную тестовую ссылку и проверить его отдельно.
- **Следит за сервером:** «Обзор» коротко показывает состояние всей панели, протоколов, пользователей и ресурсов; на «Контроле» графики нагрузки и проверки с подсказкой, что делать, если что-то не так.
- **Обновляется кнопкой** и сама откатывается, если новая версия не запустилась.

Скоро: каскад через точку маршрутизации в России (gRPC и TUIC), XHTTP через Yandex Cloud CDN.

## Быстрый старт

**1. Что нужно.** Чистый VPS с Debian 11+ или Ubuntu 20.04+ (amd64 или arm64), домен с A-записью на IP сервера и доступ root.

**2. Установка.** Подключитесь к серверу по SSH и выполните:

```bash
curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/install.sh | sudo bash
```

Установщик спросит роль сервера (выберите «Основной сервер»; «Точка маршрутизации» нужна для будущего каскада), домен с email для сертификата и логин с паролем администратора (не короче 10 символов). В конце он покажет адрес панели вида `https://<домен>/<секретный путь>/`. Сохраните его; забыли — `tigersb info` на сервере.

Если у хостинга свой файрвол, откройте в нём порты 443 и 8443 для TCP и UDP.

**3. Добавьте человека.** Откройте адрес панели, войдите, на вкладке **«Пользователи»** добавьте человека и нажмите на него: появятся ключ TigerSB и подписка https с QR-кодами.

**4. Подключите устройство.** Скачайте [TigerSB для Android](https://github.com/Stilltiger45/TigerSB-client/releases/latest/download/TigerSB-android.apk) и откройте ключ `tigersb://` на телефоне (или отсканируйте QR, или вставьте ключ в приложение) и нажмите кнопку подключения. В v2rayNG, Hiddify и других клиентах добавьте ссылку https как подписку.

**5. Спрячьте панель.** Закончили настройку — нажмите «Закрыть панель» на вкладке «Обзор». Открыть снова: `tigersb open` по SSH.

Подробно о каждой вкладке: [руководство по панели](docs/guide.md).

## Обновление и удаление

Обновить: кнопка **«Обновить панель»** на вкладке «Контроль» или `tigersb update` на сервере. Пользователи и настройки сохраняются, VPN во время обновления работает.

Удалить всё (спросит подтверждение и предложит сохранить копию настроек и базы):

```bash
curl -fsSL https://raw.githubusercontent.com/Stilltiger45/TigerSB-panel/main/scripts/uninstall.sh | sudo bash
```

<details>
<summary><b>Команды на сервере</b></summary>

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
| `tigersb singbox status` | состояние sing-box, порты TUIC, Hysteria 2, NaïveProxy и Mieru |
| `tigersb singbox apply` | заново собрать конфиг sing-box и копию сертификата, проверить и перезапустить |
| `tigersb mieru apply` | то же для контейнера Mieru (mbox) |
| `tigersb monitor` | снять показатели и проверить сервер сейчас (обычно это делает таймер) |

</details>

# Для разработчиков

## Компоненты

| Компонент | Роль | Лицензия |
|---|---|---|
| [Xray-core](https://github.com/XTLS/Xray-core) 26.9.30 | VPN-ядро: VLESS + REALITY, XHTTP, gRPC. Официальный образ в Docker, закреплён по хешу | MPL-2.0 |
| [sing-box](https://github.com/SagerNet/sing-box) 1.14.4 | второе VPN-ядро: TUIC v5, Hysteria 2 и NaïveProxy. Официальный образ в Docker, закреплён по хешу | GPL-3.0-or-later |
| [mbox](https://github.com/enfein/mbox) 1.14.3 | сборка sing-box от автора Mieru, третье ядро для Mieru. Бинарник из релиза, проверен по sha256, работает в образе sing-box | GPL-3.0-or-later |
| [Caddy](https://caddyserver.com) 2.11.4 | сайт-заглушка, сертификат Let's Encrypt, вход в панель и подписки | Apache-2.0 |
| [Docker](https://www.docker.com) | запуск Xray, sing-box и mbox без прав и с файловой системой только для чтения | Apache-2.0 |
| [Go](https://go.dev) 1.26 | панель, служба порта 443, служба подписок, сбор данных | BSD-3-Clause |
| [SQLite](https://sqlite.org) (modernc, без cgo) | пользователи, устройства, настройки, графики | BSD-3-Clause |
| [pkg/profile](https://github.com/Stilltiger45/TigerSB-client/tree/main/pkg/profile) из TigerSB-client | формат и подпись профиля TigerSB | GPL-3.0 |

## Как это работает

- **Один бинарник `tigersb`** на Go и несколько служб systemd `tigersb-*`: веб-панель, служба порта 443, подписки, Caddy, Xray, sing-box, Mieru, таймер проверок и обновление. Все данные лежат в SQLite в `/var/lib/tigersb`.
- **Порт 443 общий.** Служба `tigersb-edge` читает SNI и отправляет подключение либо в Caddy (сайт, панель, подписки), либо в нужный протокол Xray. Hysteria 2 занимает UDP 443, TUIC — UDP 8443, NaïveProxy — TCP 2443 (его держит `tigersb-edge`, чтобы включать BBR; по желанию ещё QUIC на UDP 2443), Mieru — TCP 2012–2022.
- **Панель пишет конфиги**, проверяет их самим ядром и только потом применяет. Пользователи добавляются в Xray через его API без перезапуска.
- **Подписки** отдаёт отдельная служба `tigersb-sub`, поэтому они работают, даже когда панель закрыта или спит. Приложение TigerSB получает подписанный профиль, остальные клиенты — список ссылок `vless://`.
- **Обновление** идёт отдельной службой вне панели: точка восстановления, установка, проверка, при сбое откат.

Подробно: [устройство панели](docs/architecture.md), [формат профиля и ключа](https://github.com/Stilltiger45/TigerSB-client/blob/main/docs/profile.md).

## Безопасность и защита

- **Скрытая панель.** Она слушает только `127.0.0.1`, наружу её публикует Caddy по секретному пути. `tigersb close` и `tigersb sleep` прячут и его, вернуть может только `tigersb open` по SSH. Запасной вход — SSH-туннель.
- **Вход.** Пароли хранятся как Argon2id, сессии как SHA-256 от токена. Cookie `HttpOnly`, `SameSite=Strict`, только для секретного пути (при входе по домену ещё и `Secure`), сессия живёт 12 часов. После 10 неудачных попыток с одного адреса он блокируется на 15 минут; неудачи для логина и общие только замедляют вход, а не запирают админа. Через SSH-туннель вход не ограничивается. Изменения принимаются только как JSON с того же источника, страницы отдаются с CSP и `X-Frame-Options`.
- **Подписанный профиль.** Панель один раз создаёт ключ Ed25519 и подписывает им каждый профиль. Публичный ключ входит в ключ `tigersb://`, и клиент принимает только профиль с этой подписью, поэтому подменить серверы по дороге нельзя.
- **Подписка не видна снаружи.** Неверный токен, выключенный пользователь или упавшая служба дают ту же заглушку с теми же заголовками, что и любая страница сайта. Ответы не кэшируются, заголовки `Server` и `Via` убраны.
- **Лимит устройств по HWID.** Клиент присылает `x-hwid` и данные устройства, новое устройство сверх лимита подписку не получает. IP устройства панель берёт из заголовка PROXY от службы порта 443, подделать его через `X-Forwarded-For` нельзя.
- **Изоляция ядер.** Xray, sing-box и mbox работают из официальных образов, закреплённых по хешу, без прав (`--cap-drop ALL`), с файловой системой только для чтения и без журналов, чтобы на диске не оставалось адресов пользователей.
- **Проверенные программы.** Caddy скачивается закреплённой версии со сверкой SHA-512. Сертификат выпускается через TLS-ALPN, порт 80 закрыт.

О найденной уязвимости сообщайте приватно: [SECURITY.md](SECURITY.md).

## Сборка и тесты

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

## Лицензии

Панель распространяется под **GPL-3.0**, см. [LICENSE](LICENSE). Лицензия совпадает с лицензией [TigerSB-client](https://github.com/Stilltiger45/TigerSB-client), поэтому панель подключает его пакет `pkg/profile`.

- **Зависимости Go** — только MIT и BSD, с GPL-3.0 совместимы. `scripts/check-licenses.sh` в CI падает, если у зависимости нет лицензии или она несовместима (AGPL, SSPL, BSL и подобные).
- **Программы рядом с панелью** (Xray MPL-2.0, sing-box и mbox GPL-3.0, Caddy Apache-2.0, Docker Apache-2.0) ставятся из официальных сборок без изменений и работают как отдельные программы. Код sing-box и mbox в панель не входит, панель только пишет его конфиг.
- **Заимствованные идеи.** Из [web-panel-proxy](https://github.com/POLESNIESOVETI12/web-panel-proxy) (MIT) — сертификат только через TLS-ALPN, закреплённый Caddy и схема обновления с точкой отката; из [OlConnect_manager](https://github.com/Oleglog/OlConnect_manager) (WTFPL) — установка и удаление одной командой; из [RealiTLScanner](https://github.com/XTLS/RealiTLScanner) (MPL-2.0) — сканер SNI. Код этих проектов не копировался, всё написано заново на Go.

Полный список с версиями: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) и вкладка «О панели».
