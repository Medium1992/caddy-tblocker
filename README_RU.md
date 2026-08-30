# caddy-tblocker

> Собственный образ [Caddy](https://caddyserver.com/) со встроенным TTL-блоклистом в памяти для вебхука торрент-блокера [Remnawave](https://github.com/remnawave). За CDN штатный nftables-бан на ноде бесполезен: все пакеты приходят к ней от Caddy. Этот модуль переносит бан туда, где реальный адрес клиента ещё известен, и заодно умеет рвать уже открытый туннель.

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)

[![Docker Pulls](https://img.shields.io/docker/pulls/medium1992/caddy-tblocker?logo=docker&label=docker%20pulls)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Docker Image Size](https://img.shields.io/docker/image-size/medium1992/caddy-tblocker/latest?logo=docker&label=image%20size)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Caddy](https://img.shields.io/github/v/release/caddyserver/caddy?label=Caddy&logo=caddy)](https://github.com/caddyserver/caddy/releases)
![Platforms](https://img.shields.io/badge/arch-amd64%20%7C%20arm64-blue)
[![Telegram](https://img.shields.io/badge/Telegram-group-blue?logo=telegram)](https://t.me/+96HVPF3Ww6o3YTNi)

## ✨ Возможности

- 🧱 **Нативный модуль Caddy**: без базы, Redis, сайдкаров и внешних API на пути запроса.
- ⏱️ **Временные баны по IP**: срок берётся из `blockDuration`, ограничивается `max_ttl`, просроченное чистится в фоне.
- ✂️ **Рвёт живые туннели**: с `drop_existing` бан отменяет уже выполняющиеся запросы, а не ждёт, пока клиент сам переподключится.
- 🌐 **Один адрес по всей цепочке**: вычисленный Caddy `{client_ip}` уходит в Xray и он же потом сверяется с отчётом.
- 🔒 **Внутренний listener**: вебхук и админ-роут висят на loopback и ограничены по source CIDR.
- ⚡ **Проверка первой**: директива стоит раньше любого терминального хендлера, обёртка в `route` больше не нужна.
- 🛟 **Страховка**: список `ignore`, который нельзя забанить, и ограниченный по размеру стор.
- 🔓 **Ручной разбан**: просмотр и снятие банов через внутренний админ-роут.
- 🐳 **Готовые образы**: `amd64` и `arm64` для Docker Hub и GHCR.

> [!IMPORTANT]
> Баны живут только в памяти Caddy. Рестарт или reload конфига их стирает — это сделано намеренно: кривой вебхук или ошибка в конфиге не смогут заблокировать людей дольше, чем живёт процесс.

## 🚀 Быстрый старт

```bash
docker pull medium1992/caddy-tblocker:latest
```

```text
medium1992/caddy-tblocker:latest
medium1992/caddy-tblocker:vX.Y.Z
ghcr.io/medium1992/caddy-tblocker:latest
ghcr.io/medium1992/caddy-tblocker:vX.Y.Z
```

Это drop-in замена обычного образа `caddy`: те же пути конфигов, те же тома, плюс четыре дополнительных модуля.

## ⚙️ Как это работает

1. CDN передаёт запрос в Caddy и кладёт реальный адрес клиента в заголовок вроде `X-Real-IP`.
2. Caddy вычисляет из него `{client_ip}` — но только потому, что TCP-пир попал в `trusted_proxies`.
3. Caddy отправляет этот же `{client_ip}` в Xray как `X-Forwarded-For` вместе с маркерным заголовком, которому Xray доверяет.
4. Xray использует этот адрес как источник соединения. Когда срабатывает bittorrent-правило, нода репортит именно его.
5. Нода POST-ит отчёт на внутренний роут `tblocker_webhook`.
6. `tblocker` сохраняет адрес, рвёт всё, что у этого клиента сейчас открыто, и отвечает `403` на всё последующее.

Отчёт, который шлёт нода, принимается как есть:

```json
{
  "actionReport": {
    "blocked": true,
    "ip": "203.0.113.42",
    "blockDuration": 3600,
    "willUnblockAt": "2026-08-28T13:00:00.000Z",
    "userId": "user@example.test"
  },
  "xrayReport": { "source": "203.0.113.42:0", "email": "user@example.test" }
}
```

Приоритет у `blockDuration` (в секундах). `willUnblockAt` — только запасной вариант, потому что это отметка по часам ноды: если часы контейнеров разъезжаются, относительная длительность всё равно даст правильный срок, а абсолютная метка сдвинет бан или сделает его просроченным прямо при получении. `default_ttl` используется, когда непригодны оба поля, а `max_ttl` ограничивает любой принятый срок.

### Что именно останавливает бан

Любой бан сразу отбивает **следующий** запрос клиента. Что произойдёт с уже идущей сессией, зависит от `drop_existing`:

**Без `drop_existing`** установленный туннель не трогается, и скорость срабатывания целиком зависит от транспорта:

| Транспорт Xray | Когда бан сработает |
|---|---|
| XHTTP `packet-up` | **Сразу.** Каждый кусок аплинка — отдельный POST, и каждый получает отказ. |
| XHTTP `stream-up` / `stream-one` | При переподключении: сессия — это один долгоживущий запрос, уже прошедший проверку. |
| WebSocket, HTTPUpgrade | При переподключении, по той же причине. |
| gRPC | При переподключении. |

**С `drop_existing`** бан дополнительно отменяет все запросы, которые у этого клиента открыты прямо сейчас, на всех перечисленных транспортах. Caddy закрывает соединение с апстримом, когда контекст запроса завершён, — в том числе для hijack-нутых WebSocket и HTTPUpgrade, — так что туннель умирает немедленно, а не доживает до момента, когда клиент сам решит переподключиться.

Отмена идёт **по запросу, а не по TCP-соединению**. За CDN это принципиально: одно HTTP/2-соединение до origin может нести сразу несколько несвязанных абонентов, и закрытие сокета уронило бы их всех, а отмена запроса убивает только потоки нарушителя.

> [!WARNING]
> Вся схема держится на том, что Xray читает `X-Forwarded-For`, а делает он это только для транспортов поверх HTTP: `websocket`, `httpupgrade`, `splithttp`/XHTTP и `grpc`. На голом TCP + TLS inbound читать нечего, Xray репортит адрес того, кто открыл TCP-соединение, то есть Caddy, и отчёт бесполезен.

## 🐳 Развёртывание

### Host networking — рекомендуемый вариант

Документация Remnawave запускает ноду с [`network_mode: host`](https://docs.rw/install/remnawave-node/) — это нужно её nftables-плагинам. Помести Caddy в тот же сетевой namespace, и вся схема сворачивается до loopback: не нужна отдельная сеть, не нужны статические адреса, а внутренний listener по построению недоступен снаружи машины.

```yaml
services:
  remnanode:
    container_name: remnanode
    hostname: remnanode
    image: remnawave/node:latest
    restart: always
    network_mode: host
    # Плагин торрент-блокера включается только при доступном nftables.
    cap_add:
      - NET_ADMIN
    environment:
      - NODE_PORT=2222
      - SECRET_KEY=supersecretkey

  caddy:
    container_name: caddy
    hostname: caddy
    image: medium1992/caddy-tblocker:latest
    restart: always
    network_mode: host
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config

volumes:
  caddy-data:
  caddy-config:
```

При host networking нет никаких `ports:`, которые можно забыть закрыть: Caddy слушает `:443` на хосте, внутренний listener привязан к `127.0.0.1:9080` (см. директиву `bind` в Caddyfile ниже), и нода ходит к нему по `http://127.0.0.1:9080/…`.

### Общая bridge-сеть

Если нода уже крутится в user-defined сети, та же схема работает на адресах контейнеров вместо loopback. Заменить нужно везде: привязать внутренний listener к адресу Caddy в этой сети, выставить `allow` в CIDR сети и направить `webhookUrl` на адрес контейнера Caddy.

```yaml
services:
  caddy:
    image: medium1992/caddy-tblocker:latest
    restart: unless-stopped
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config
    ports:
      - "443:443"
    # 9080 не публикуется никогда: нода ходит туда только по этой сети.
    networks:
      remnawave-network:
        ipv4_address: 192.168.243.2

  remnanode:
    image: remnawave/node:latest
    cap_add:
      - NET_ADMIN
    networks:
      remnawave-network:
        ipv4_address: 192.168.243.3

networks:
  remnawave-network:
    external: true

volumes:
  caddy-data:
  caddy-config:
```

> [!CAUTION]
> В bridge-сети внутренний listener доступен любому другому контейнеру, подключённому к этой сети, и `allow` — твоя единственная защита. При host networking он привязан к loopback и доступен только с самой машины. Если нет причин поступать иначе, выбирай host networking.

## 🧩 Caddyfile

Написан под host networking; для bridge-сети замени loopback-адреса, как описано выше.

```caddy
{
	tblocker {
		default_ttl 1m
		max_ttl 24h

		# Никогда не банить инфраструктуру. При host networking запасной адрес
		# Xray — это loopback, и защита от его бана не даёт кривому отчёту
		# положить origin.
		ignore 127.0.0.0/8
	}

	# Доверяем только задокументированным диапазонам CDN.
	servers :443 {
		trusted_proxies static 198.51.100.0/24 2001:db8:1234::/48
		client_ip_headers X-Real-IP
	}
}

# Именно `bind` ограничивает сокет петлёй. Один только адрес сайта
# `http://127.0.0.1:9080` добавляет лишь матчер по Host — слушать Caddy всё
# равно будет на всех интерфейсах.
http://127.0.0.1:9080 {
	bind 127.0.0.1

	@node_webhook path /internal/tblocker/replace-with-a-long-random-secret
	handle @node_webhook {
		tblocker_webhook {
			allow 127.0.0.0/8
			max_body 64KiB
		}
	}

	@tblocker_admin path /internal/tblocker/admin
	handle @tblocker_admin {
		tblocker_admin {
			allow 127.0.0.0/8
		}
	}

	respond 404

	# Иначе секретный путь попадёт в access-лог целиком.
	log {
		output discard
	}
}

example.com {
	# Обёртка `route` не нужна: директива уже стоит раньше любого хендлера,
	# который может завершить цепочку, включая `handle` и `respond`.
	tblocker {
		status 403
		drop_existing
	}

	reverse_proxy 127.0.0.1:10000 {
		# Маркер, по которому Xray соглашается доверять переданному адресу.
		header_up X-Trusted-Proxy "caddy"
		# Отправляем ровно тот адрес, который tblocker потом и проверит.
		header_up X-Forwarded-For {client_ip}
	}
}
```

> [!TIP]
> В прошлых версиях этого README сайт оборачивался в `route { ... }`. Больше не нужно: `tblocker` зарегистрирован перед `redir`, а значит идёт раньше `handle`, `handle_path`, `route`, `respond`, `error`, `abort` и `reverse_proxy`. Уже написанный `route` продолжит работать как работал.

## 🔌 Плагин ноды и Xray

Поле `webhookUrl` в плагине торрент-блокера доступно начиная с **node v3.1.0**. Ниже — полный блок `torrentBlocker` для **конфигурации плагина ноды**. Его место в плагинах ноды, а не в Caddyfile и не в клиентском профиле:

```json
{
  "torrentBlocker": {
    "enabled": true,
    "webhookUrl": "http://127.0.0.1:9080/internal/tblocker/replace-with-a-long-random-secret",
    "ignoreLists": {
      "ip": ["127.0.0.1"],
      "userId": []
    },
    "blockDuration": 3600
  }
}
```

| Поле | Что указывать |
|---|---|
| `enabled` | `true` включает детект торрентов и правило маршрутизации Xray на ноде. |
| `webhookUrl` | Внутренний URL Caddy. При host networking это `127.0.0.1:9080`, в bridge-сети — адрес контейнера Caddy. Случайный путь обязан в точности совпадать с матчером в Caddyfile. |
| `blockDuration` | Длительность блокировки в **секундах**, `3600` — час. Указывай положительное значение. `0` означает вечный бан в nftables на ноде, а стор в памяти такого выразить не может, поэтому Caddy откатится на `default_ttl`. |
| `ignoreLists.ip` | Адреса, которые нельзя блокировать. **Укажи здесь адрес самого Caddy** — при host networking это `127.0.0.1`. Сравнение идёт по точной строке, поэтому CIDR не сработает: нужен именно адрес. |
| `ignoreLists.userId` | Идентификаторы пользователей Xray (поле `email` у пользователя inbound), которых нельзя блокировать. |

> [!NOTE]
> Плагин включается только при доступном сервисе nftables, так что контейнеру ноды нужна capability `CAP_NET_ADMIN`. Без неё отчёт не формируется вообще и до Caddy ничего не доходит.

Обрати внимание: вебхук вызывает именно **нода**, напрямую, в момент срабатывания правила в её Xray. Панель тут не участвует — она ни отчёт не отправляет, ни о сохранённом в Caddy бане не узнаёт.

В **inbound** Xray, который принимает трафик от Caddy, объяви маркерный заголовок:

```json
{
  "streamSettings": {
    "sockopt": {
      "trustedXForwardedFor": ["X-Trusted-Proxy"]
    }
  }
}
```

Xray принимает `X-Forwarded-For` только когда присутствует один из перечисленных в `trustedXForwardedFor` заголовков. Caddy отправляет `X-Trusted-Proxy: caddy`, поэтому `X-Trusted-Proxy` обязан быть в списке. Имя маркера произвольное, но обе стороны должны договориться. К конфигурации плагина это отношения не имеет: именно эта пара настроек делает адрес в отчёте Xray равным реальному адресу клиента, который потом проверит Caddy.

## 🔐 Реальный IP клиента

`{client_ip}` — это вычисленный самим Caddy адрес клиента. Это не заголовок и не TCP-адрес, который видит Xray. Caddy берёт его из заголовка CDN **только после** того, как TCP-пир совпал с `trusted_proxies`; иначе это адрес самого пира.

- Замени примерные CIDR на задокументированные диапазоны своего CDN.
- Закрой origin фаерволом, чтобы до публичного listener'а доставал только CDN.
- Никогда не ставь `trusted_proxies static 0.0.0.0/0` на публичном listener: тогда заголовок сможет подделать любой, кто достучится напрямую.

> [!CAUTION]
> Передавай `{client_ip}`, а не сырой заголовок. `header_up X-Forwarded-For {http.request.header.X-Real-Ip}` выглядит эквивалентно, но это не так: он копирует непроверенный заголовок вообще без проверки доверия. Любой, кто достучится до origin напрямую, поставит `X-Real-IP` с адресом жертвы, и в отчёт уйдёт именно он — чужой IP забанен, сам атакующий невидим. А если заголовка просто нет, Caddy выставит пустой `X-Forwarded-For`, Xray откатится на TCP-пира, и нода отрепортит **собственный адрес Caddy**, который её же ingress-фильтр nftables и заблокирует. С `{client_ip}` ни то ни другое невозможно.

Если `trusted_proxies` не настроен вовсе, `{client_ip}` будет адресом edge-узла CDN: у всех абонентов за этим узлом окажется одно значение, и один отчёт заблокирует их всех разом.

## 🔓 Как снять бан

Команда «разблокировать» в панели идёт на nftables-эндпоинт ноды и до Caddy не доходит, поэтому снимать бан нужно здесь. Для этого есть роут `tblocker_admin`, доступный с самой машины:

```bash
# Показать все активные баны
curl http://127.0.0.1:9080/internal/tblocker/admin
# {"count":1,"bans":[{"ip":"203.0.113.42","expires_at":"2026-08-28T13:00:00Z"}]}

# Снять один адрес
curl -X DELETE 'http://127.0.0.1:9080/internal/tblocker/admin?ip=203.0.113.42'
# {"removed":1}

# Очистить весь блоклист
curl -X DELETE http://127.0.0.1:9080/internal/tblocker/admin
# {"removed":3}
```

Снятие адреса без активного бана вернёт `404`, некорректный адрес — `400`. Рестарт Caddy тоже очищает всё.

## 🛠️ Директивы

### Глобальное приложение

```caddy
tblocker {
	default_ttl    <duration>
	max_ttl        <duration>
	sweep_interval <duration>
	max_entries    <int>
	ipv4_prefix    <1-32>
	ipv6_prefix    <1-128>
	ignore         <CIDR> [<CIDR>...]
}
```

| Опция | По умолчанию | Описание |
|---|---:|---|
| `default_ttl` | `1m` | Срок бана, когда в отчёте нет пригодной длительности. |
| `max_ttl` | `24h` | Верхняя граница для любого принятого бана. |
| `sweep_interval` | `1m` | Как часто в фоне вычищается просроченное. Забаненный адрес обычно перестаёт подключаться, поэтому без фоновой чистки его запись висела бы до следующего reload. |
| `max_entries` | `100000` | Ограничение размера стора. При переполнении сначала освобождаются просроченные записи; если места всё равно нет, отчёт логируется и отбрасывается. |
| `ipv4_prefix` | `32` | Ширина бана для IPv4. `24` забанит всю `/24`. |
| `ipv6_prefix` | `128` | Ширина бана для IPv6. Если у тебя есть IPv6-клиенты, осмысленное значение — `64`: один хост меняет адрес в пределах своей `/64`. |
| `ignore` | нет | CIDR, которые никогда не банятся и никогда не блокируются. |

### Хендлер запроса

```caddy
tblocker {
	status 403
	drop_existing
}
```

| Опция | По умолчанию | Описание |
|---|---:|---|
| `status` | `403` | Ответ заблокированному клиенту. Допустимо `400`–`599`, всё остальное отклоняется при загрузке конфига. |
| `drop_existing` | выкл | Отменять уже выполняющиеся запросы забаненного адреса. Без неё установленный туннель доживает до переподключения клиента. Принимает голый флаг либо `on` / `off`. |

Запрос, у которого `{client_ip}` пуст или не парсится, пропускается, а не блокируется.

### Хендлер вебхука

```caddy
tblocker_webhook {
	allow    <CIDR> [<CIDR>...]
	max_body <размер>
}
```

| Опция | По умолчанию | Описание |
|---|---:|---|
| `allow` | обязательна | CIDR, которым разрешено слать отчёты. Сверяется с реальным TCP-пиром, а не с проксирующим заголовком. |
| `max_body` | `64KiB` | Максимальный принимаемый размер тела запроса. |

Сохранённый отчёт возвращает `204`. Отчёт на адрес из `ignore` тоже вернёт `204`, попадёт в лог и ничего не сохранит. Некорректные данные — `400`, вызов извне `allow` — `403`.

### Админ-хендлер

```caddy
tblocker_admin {
	allow <CIDR> [<CIDR>...]
}
```

| Метод | Действие |
|---|---|
| `GET` | Отдаёт `{"count":N,"bans":[{"ip":…,"expires_at":…}]}`. |
| `DELETE ?ip=<адрес>` | Снимает один адрес. `200`, если бан был активен, `404`, если нет. |
| `DELETE` | Очищает блоклист и возвращает число снятых активных записей. |

Защищай этот роут так же, как вебхук: неугадываемый путь, привязка к loopback и список `allow`.

## 🔎 Проверка цепочки

Если баны не появляются, сначала выясни, какое звено сломано:

1. **Видит ли нода реальный адрес клиента?** В её логе есть строка `[TORRENT-BLOCKER] IP: <адрес>, user: …`. Если там адрес Caddy — неверно настроен `trustedXForwardedFor` или `header_up`. Чини это первым, всё остальное зависит от него.
2. **Доходит ли отчёт?** Caddy пишет `torrent client temporarily blocked` с полями `client_ip`, `expires_at` и `dropped_requests`. Пусто — значит неверен URL вебхука, секретный путь или список `allow`. Нода ошибки вебхука глотает молча и не подскажет.
3. **Работает ли `drop_existing`?** Поле `dropped_requests` в той же строке — это число разорванных живых запросов. Стабильный `0` при явно открытых сессиях означает, что бан ложится не на тот адрес, который вычисляет Caddy: возвращайся к пункту 1.
4. **Тот же ли адрес вычисляет Caddy?** Сравни бан из `tblocker_admin` с `{client_ip}` в access-логах. Расходятся — значит `trusted_proxies` / `client_ip_headers` не соответствуют тому, что реально шлёт CDN.

## 🏗️ Локальная сборка

```bash
docker build -t caddy-tblocker:local .
docker run --rm caddy-tblocker:local caddy version
```

Образ сохраняет стандартные модули Caddy и добавляет:

```text
tblocker
http.handlers.tblocker
http.handlers.tblocker_webhook
http.handlers.tblocker_admin
```

## 🔄 Автоматические сборки

Workflow запускается вручную и раз в сутки проверяет апстрим. Ручной запуск собирает всегда, запуск по расписанию публикует только при смене релиза Caddy или тулчейна Go. Go-бинарь кросс-компилируется под `amd64` и `arm64`, образы уезжают в Docker Hub и GHCR.

Для публикации в Docker Hub добавь секреты `DOCKERHUB_USERNAME` и `DOCKERHUB_TOKEN`, а в настройках GitHub Actions выстави **Workflow permissions** в **Read and write**, чтобы сборки по расписанию могли обновлять файл с версией апстрима.

## ⭐ Поддержка

Если проект оказался полезен — поставь [звезду](https://github.com/Medium1992/caddy-tblocker/stargazers) и заходи в [Telegram-группу](https://t.me/+96HVPF3Ww6o3YTNi).

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)
