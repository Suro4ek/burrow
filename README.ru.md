# burrow

Self-hosted туннель-сервер: открыть наружу сервис, который крутится на ноутбуке,
или зайти на него по SSH — через собственный VPS. Как ngrok, только сервер твой,
и с веб-панелью для выдачи токенов.

*[English version](README.md)*

```console
$ burrow http 3000
connected to tun.example.com:7000

  http  https://myapp.tun.example.com

$ burrow ssh
connected to tun.example.com:7000

  tcp   tun.example.com:25343
        ssh -p 25343 you@tun.example.com
```

- **HTTP-туннели** на wildcard-сабдоменах, websockets и SSE работают сразу
- **TCP-туннели** на публичных портах — SSH, Postgres, что угодно
- **Стабильный порт для SSH**: зарезервируй порт за токеном один раз, и
  `burrow ssh` всегда попадает на него — `~/.ssh/config` продолжает работать
- **Веб-панель** для выдачи и отзыва токенов и наблюдения за туннелями,
  вкомпилирована в бинарник сервера
- Одна Go-зависимость (`hashicorp/yamux`), сервер — один статический бинарник

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/panel-tunnels-dark.png">
  <img alt="Панель со списком четырёх живых туннелей: публичные адреса, локальные цели, токены-владельцы, число соединений и трафик" src="docs/panel-tunnels-light.png">
</picture>

## Установка

**Агент** (машина, которую открываем):

```sh
go install github.com/suro4ek/burrow/cmd/burrow@latest
```

**Сервер** (VPS):

```sh
go install github.com/suro4ek/burrow/cmd/burrowd@latest
```

Либо готовые бинарники из [Releases](https://github.com/suro4ek/burrow/releases),
либо контейнер:

```sh
docker pull ghcr.io/suro4ek/burrow:latest
```

## Попробовать локально за две минуты

`lvh.me` и все его поддомены резолвятся в `127.0.0.1`, так что DNS трогать не
надо.

```sh
# Терминал 1 — сервер
BURROWD_ADMIN_PASSWORD=devpassword burrowd \
  -domain lvh.me -http 127.0.0.1:8080 -control 127.0.0.1:7000 \
  -tokens tokens.json -admin-addr 127.0.0.1:7002
```

Открой <http://127.0.0.1:7002/_admin/>, зайди с паролем `devpassword`, нажми
**New token** — панель покажет готовую строку `burrow login`.

```sh
# Терминал 2 — агент
burrow login -server 127.0.0.1:7000 -token <токен> -no-tls
python3 -m http.server 3000 &
burrow http 3000 -subdomain demo

curl http://demo.lvh.me:8080/
```

## Развёртывание на VPS

### 1. DNS

```
tun.example.com.    A    203.0.113.10
*.tun.example.com.  A    203.0.113.10
```

### 2. Сертификаты

Нужен **wildcard**-сертификат, а его **нельзя** получить через HTTP-01
challenge — только через DNS-01, то есть требуется API-токен DNS-провайдера.
Проще всего отдать это Caddy, а он проксирует на burrowd по loopback: рабочий
пример в [`deploy/Caddyfile`](deploy/Caddyfile).

Control-порту (7000) нужен свой TLS: агенты шлют туда токен. Укажи
`-tls-cert` / `-tls-key` на тот же сертификат. Перезапуск при обновлении не
нужен — burrowd перечитывает файлы, когда меняется их mtime.

### 3. Запуск

Через systemd — [`deploy/burrowd.service`](deploy/burrowd.service) готов к
копированию:

```sh
scp burrowd root@vps:/usr/local/bin/
scp deploy/burrowd.service root@vps:/etc/systemd/system/

ssh root@vps '
  useradd --system --no-create-home burrowd
  mkdir -p /etc/burrowd/tls
  openssl rand -base64 24 > /etc/burrowd/admin-password
  chown -R burrowd:burrowd /etc/burrowd
  chmod 600 /etc/burrowd/admin-password
  systemctl enable --now burrowd
'
```

Либо через Docker — см. [`docker-compose.yml`](docker-compose.yml). Используй
host networking: пул TCP-туннелей — это тысяча портов, и публиковать такой
диапазон через userland-прокси медленно и прожорливо по памяти.

`tokens.json` создаётся при первом запуске, дальше токены заводятся в панели.

### 4. Firewall

```sh
ufw allow 80,443/tcp
ufw allow 7000/tcp          # control
ufw allow 20000:30000/tcp   # пул TCP-туннелей
```

## Команды агента

```sh
burrow login -server tun.example.com:7000 -token TOKEN   # один раз

burrow http 3000                     # случайный сабдомен
burrow http 3000 -subdomain myapp    # https://myapp.tun.example.com
burrow http localhost:8080
burrow tcp 5432                      # порт из пула
burrow ssh                           # локальный :22, печатает ssh-команду
burrow ssh -port 25343               # конкретный порт (должен быть зарезервирован)
burrow start -tunnel http:3000:myapp -tunnel tcp:22   # несколько за раз

burrow config                        # показать сохранённый логин
burrow logout
```

Логин лежит в `~/.config/burrow/config.json` с правами `600`. Приоритет: явный
флаг → сохранённый логин → `BURROW_SERVER` / `BURROW_TOKEN`.

Голый номер порта означает `127.0.0.1`, а не `0.0.0.0` — чтобы случайно не
опубликовать чужой сервис из локальной сети.

Компактный формат `-tunnel`:

| Спецификация | Значение |
|---|---|
| `http:3000` | локальный `127.0.0.1:3000`, случайный сабдомен |
| `http:3000:myapp` | сабдомен `myapp` |
| `http:localhost:8080` | локальный `localhost:8080` |
| `tcp:22` | локальный `127.0.0.1:22`, порт по правилам ниже |
| `tcp:22:25343` | публичный порт `25343` |

## Веб-панель

Включается наличием пароля и живёт на **голом базовом домене**:
`https://tun.example.com/_admin/`. Сабдомены отданы туннелям, так что ни один
туннель её не перекроет.

Пароль берётся из `-admin-password-file` или `BURROWD_ADMIN_PASSWORD` и
намеренно **не** из флага: аргументы командной строки видны в `ps` любому
пользователю машины. В памяти держится только PBKDF2-хеш.

- **Tunnels** — живой список: публичный адрес, локальный, токен-владелец, число
  соединений, трафик in/out, возраст.
- **Tokens** — создание с резервами сабдоменов и портов, лимитом туннелей и
  запретом TCP; редактирование, disable, rotate, delete. После создания панель
  показывает готовую команду `burrow login` с кнопкой копирования.
- **Agents** — подключённые агенты и принудительное отключение.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/panel-tokens-dark.png">
  <img alt="Вкладка Tokens: три токена со скрытыми секретами, зарезервированные сабдомены и порты, лимиты и действия" src="docs/panel-tokens-light.png">
</picture>

Изменения применяются сразу: `disable`, `rotate` и `delete` рвут соединения
агентов этого токена, а не ждут переподключения. Лимиты и резервы перечитываются
из стора при каждом запросе туннеля, а не фиксируются на момент handshake.

Чтобы не держать панель в открытом интернете, не задавай пароль на публичном
хосте, а подними её на loopback:

```sh
burrowd ... -admin-addr 127.0.0.1:7002
ssh -L 7002:127.0.0.1:7002 root@vps    # и открой http://127.0.0.1:7002/_admin/
```

## Токены

`tokens.json` перезаписывается сервером при изменениях через панель, атомарно
(временный файл, `fsync`, `rename`) — падение посреди записи не оставит
обрезанный список:

```json
[
  {
    "id": "k3m9x2ptqa",
    "token": "секрет",
    "name": "laptop",
    "subdomains": ["dev", "api"],
    "ports": [25343],
    "max_tunnels": 8,
    "created_at": "2026-08-08T10:00:00Z"
  }
]
```

`subdomains` и `ports` — это **резервирование**: имена закреплены за токеном, и
другой токен их не займёт. По умолчанию любой аутентифицированный агент может
взять любое свободное имя, `-free-subdomains=false` ограничивает каждого своими.
С фиксированными TCP-портами наоборот: запросить конкретный порт можно, только
если он зарезервирован за тобой, если не включён `-free-ports`.

Файл можно править руками, но он читается при старте — после правки нужен
рестарт. Записи старого формата (без `id`) мигрируются автоматически.

## Как это работает

Соединение всегда инициирует агент — у него нет белого IP, у VPS есть. Внутри
одного долгоживущего TCP/TLS-соединения yamux даёт много независимых логических
стримов.

```
браузер ──▶ :443 ─▶ Caddy ─▶ burrowd :8080 ─┐  роутинг по Host: abc.tun.example.com
                                             ├─▶ сессия агента #7
ssh ──────▶ :25343 ────────▶ burrowd ────────┘  роутинг по номеру порта
                                             │
                                             │  одно соединение, yamux-стримы
                                             ▼
                                    агент burrow ──▶ localhost:3000 / localhost:22
```

На каждое входящее соединение сервер открывает новый стрим, пишет заголовок
`StreamOpen`, агент дозванивается до локального сервиса и отвечает `StreamAck`
— дальше это два `io.Copy`.

HTTP-туннели идут через `httputil.ReverseProxy`, у которого `DialContext`
вместо реального дозвона открывает yamux-стрим. Это бесплатно даёт keep-alive,
websockets и корректные `X-Forwarded-*`.

```
cmd/burrowd/      сервер
cmd/burrow/       агент
internal/proto/   протокол: длина + JSON
internal/server/  control-листенер, HTTP-роутер, пул TCP-портов, токен-стор, админ-API
internal/client/  реконнект, сохранённый логин, спецификации туннелей
web/              панель на Vite + React, вкомпилирована через go:embed
deploy/           systemd unit, Caddyfile, пример tokens.json
```

## Флаги сервера

| Флаг | По умолчанию | Что делает |
|---|---|---|
| `-domain` | — | wildcard-зона, обязателен |
| `-control` | `:7000` | листенер для агентов |
| `-http` | `:80` | листенер пользовательского трафика |
| `-scheme` | `http` | схема в публикуемых URL, за Caddy ставь `https` |
| `-tcp-range` | `20000-30000` | пул публичных TCP-портов |
| `-tls-cert` / `-tls-key` | — | TLS на control-порту |
| `-tokens` | `tokens.json` | файл токенов |
| `-admin-password-file` | — | включает панель (или `BURROWD_ADMIN_PASSWORD`) |
| `-admin-addr` | — | дополнительный листенер панели, напр. `127.0.0.1:7002` |
| `-status-token` | — | включает `GET /_status` на базовом домене |
| `-free-subdomains` | `true` | разрешить любому агенту брать свободные сабдомены |
| `-free-ports` | `false` | разрешить любому агенту просить свободные фикс-порты |
| `-max-tunnels` | `16` | лимит туннелей на одно соединение агента |
| `-log-level` | `info` | `debug` логирует каждый стрим |

## Чем отличается от других

- **[frp](https://github.com/fatedier/frp)** — заметно больше возможностей
  (P2P, плагины, много протоколов) и больше конфигурации. Бери его, если нужна
  широта.
- **[sish](https://github.com/antoniomika/sish)** — транспорт на SSH, клиенту
  вообще ничего не надо ставить. Бери, если агент развернуть нельзя.
- **[rathole](https://github.com/rapiz1/rathole)** / **[bore](https://github.com/ekzhang/bore)**
  — компактнее и быстрее, но без панели и управления токенами.
- **ngrok / tuna.am** — хостед, ничего не надо поднимать, но сервер не твой.

Ниша burrow: маленький, читаемый, один бинарник, с управлением токенами и
панелью из коробки.

## Разработка

См. [CONTRIBUTING.md](CONTRIBUTING.md). Коротко:

```sh
make web      # собрать панель (Node 24)
make build    # bin/burrow и bin/burrowd
make race     # главный тестовый прогон
```

## Чего здесь нет

Осознанно за рамками: ACME внутри burrowd (этим занимается Caddy), инспектор
запросов как у ngrok, UDP-туннели, rate limiting и квоты по трафику,
кластеризация — состояние туннелей живёт в памяти одного процесса, поэтому
рестарт сервера рвёт туннели, а агенты переподключаются с backoff. Токены
переживают рестарт, они на диске.

## Лицензия

[MIT](LICENSE)
