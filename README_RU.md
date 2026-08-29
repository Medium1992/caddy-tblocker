# caddy-tblocker

> Кастомный образ [Caddy](https://caddyserver.com/) с нативным TTL-блоклистом в памяти для webhook от torrent-blocker в [Remna](https://github.com/remnawave). Получив IP от Remna, Caddy блокирует клиента на HTTP-уровне до передачи запроса в Xray.

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)

[![Docker Pulls](https://img.shields.io/docker/pulls/medium1992/caddy-tblocker?logo=docker&label=docker%20pulls)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Docker Image Size](https://img.shields.io/docker/image-size/medium1992/caddy-tblocker/latest?logo=docker&label=image%20size)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Caddy](https://img.shields.io/github/v/release/caddyserver/caddy?label=Caddy&logo=caddy)](https://github.com/caddyserver/caddy/releases)
![Platforms](https://img.shields.io/badge/arch-amd64%20%7C%20arm64-blue)
[![Telegram](https://img.shields.io/badge/Telegram-group-blue?logo=telegram)](https://t.me/+96HVPF3Ww6o3YTNi)

## ✨ Возможности

- 🧱 **Нативный модуль Caddy**: без базы данных, Redis, sidecar-контейнера и внешнего API на пути запроса.
- ⏱️ **Временные блокировки IP**: принимает `willUnblockAt` или `blockDuration` из Remna и лениво удаляет истёкшие записи.
- 🌐 **Корректный IP клиента**: использует `{client_ip}` от Caddy с учётом trusted proxies, а не сырой заголовок из запроса.
- 🔒 **Внутренний webhook**: отправителя можно ограничить Docker-подсетью, а порт `9080` не публиковать наружу.
- ⚡ **Ранний отказ**: проверка выполняется до reverse proxy в Xray.
- 🐳 **Готовые образы**: multi-arch `amd64` и `arm64` в Docker Hub и GHCR.
- 🔄 **Отслеживание upstream**: scheduled workflow пересобирает образ при обновлении Caddy или Go.

> [!IMPORTANT]
> Блокировки хранятся только в памяти Caddy. Перезапуск или reload Caddy намеренно их очищает: ошибочный webhook или конфиг не оставит клиентов заблокированными навсегда.

## 🚀 Быстрый старт

```bash
docker pull medium1992/caddy-tblocker:latest
```

Используй образ вместо стандартного `caddy` в Compose:

```yaml
services:
  caddy:
    image: medium1992/caddy-tblocker:latest
    restart: unless-stopped
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config
    # Не публикуй 9080: Remna обращается к нему только внутри сети.
    ports:
      - "443:443"
```

Имена образов:

```text
medium1992/caddy-tblocker:latest
medium1992/caddy-tblocker:vX.Y.Z
ghcr.io/medium1992/caddy-tblocker:latest
ghcr.io/medium1992/caddy-tblocker:vX.Y.Z
```

## ⚙️ Как это работает

1. Remna делает POST с отчётом torrent-blocker на внутренний `tblocker_webhook`.
2. Webhook сохраняет IP и время истечения в памяти Caddy.
3. `tblocker` сравнивает `{client_ip}` от Caddy с этим списком.
4. При совпадении Caddy возвращает `403`, не передавая запрос в Xray или другой upstream.

Текущий payload от Remna принимается без преобразований:

```json
{
  "actionReport": {
    "ip": "203.0.113.42",
    "blockDuration": 60,
    "willUnblockAt": "2026-08-28T12:01:00.000Z"
  }
}
```

Приоритет у `willUnblockAt`. Если его нет, `blockDuration` трактуется как секунды. `default_ttl` применяется, только когда в payload нет ни одного значения; `max_ttl` ограничивает любой присланный срок.

> [!NOTE]
> Актуальные ноды Remna отправляют torrent-отчёты, только когда доступен их сервис nftables. Оставь ноде требуемую nft capability: после отправки webhook Caddy становится клиентским уровнем блокировки.

## 🧩 Caddyfile

`route` здесь использован намеренно: он сохраняет явно заданный порядок директив и гарантирует, что бан проверяется раньше terminal-директив, включая `reverse_proxy`.

```caddy
{
  tblocker {
    default_ttl 1m
    max_ttl 24h
  }

  # Укажи только реальные диапазоны ingress-узлов CDN.
  servers :443 {
    trusted_proxies static 198.51.100.0/24 2001:db8:1234::/48
    client_ip_headers X-Real-IP
  }
}

# Только Docker-сеть. Не публикуй порт 9080 в Интернет.
http://:9080 {
  @remna_webhook path /internal/tblocker/replace-with-a-long-random-secret
  handle @remna_webhook {
    tblocker_webhook {
      allow 192.168.243.0/28
      max_body 64KB
    }
  }

  respond 404
}

example.com {
  route {
    # Должен быть первым: проверяет IP клиента до Xray.
    tblocker {
      status 403
    }

    reverse_proxy 192.168.243.3:10000
  }
}
```

В Remna укажи точный URL webhook:

```text
http://caddy:9080/internal/tblocker/replace-with-a-long-random-secret
```

Подставь вместо `caddy` имя сервиса из Compose. Если между контейнерами используются закреплённые IP вместо Docker DNS, укажи внутренний статичный IP Caddy.

## 🔐 Реальный IP клиента

Модуль **не доверяет** `X-Real-IP` и `X-Forwarded-For` сам по себе. Он получает уже вычисленный Caddy адрес клиента. Поэтому на listener, принимающем трафик от CDN, обязательно нужны `trusted_proxies` и `client_ip_headers`.

- Замени пример CIDR на опубликованные диапазоны ingress-узлов своего CDN.
- Ограничь origin фаерволом: к публичному listener должны приходить только IP CDN.
- Не ставь `trusted_proxies static 0.0.0.0/0` на публичном listener: прямой клиент сможет подделать заголовок.
- Для отдельного CDN-only порта, закрытого фаерволом, trusted-proxy policy можно задать отдельно.

Без этого Caddy увидит IP edge-узла CDN, а IP абонента из отчёта Remna никогда не совпадёт с ним.

## 🛠️ Директивы

### Глобальное приложение

```caddy
tblocker {
  default_ttl <Go duration>
  max_ttl <Go duration>
}
```

| Параметр | По умолчанию | Описание |
|---|---:|---|
| `default_ttl` | `1m` | Резервный срок, если webhook не прислал валидное время блокировки. |
| `max_ttl` | `24h` | Максимальный разрешённый срок блокировки. |

### Обработчик запросов

```caddy
tblocker {
  status 403
}
```

| Параметр | По умолчанию | Описание |
|---|---:|---|
| `status` | `403` | HTTP-статус для заблокированного клиента. Допускается только 4xx. |

### Обработчик webhook

```caddy
tblocker_webhook {
  allow <CIDR> [<CIDR>...]
  max_body <bytes>
}
```

| Параметр | По умолчанию | Описание |
|---|---:|---|
| `allow` | обязательно | CIDR, которым разрешено вызывать webhook. |
| `max_body` | `64KB` | Максимальный размер тела webhook-запроса. |

Успешный webhook получает `204 No Content`. Некорректный payload получает `400`; адрес вне `allow` получает `403`.

## 🏗️ Локальная сборка

```bash
docker build -t caddy-tblocker:local .
docker run --rm caddy-tblocker:local caddy version
```

Образ сохраняет стандартные модули Caddy и добавляет:

```text
http.handlers.tblocker
http.handlers.tblocker_webhook
```

## 🔄 Автоматические сборки

GitHub Actions workflow можно запускать вручную; раз в сутки он также проверяет upstream. Ручной запуск всегда собирает образ, scheduled-запуск публикует его только при изменении Caddy или Go. Образы нативно собираются для `amd64` и `arm64`, затем публикуются в Docker Hub и GHCR.

Для публикации в Docker Hub добавь repository secrets `DOCKERHUB_USERNAME` и `DOCKERHUB_TOKEN`. В GitHub Actions включи **Workflow permissions**: **Read and write**, чтобы scheduled-сборка могла обновлять отслеживаемый файл версий upstream.

## ⭐ Поддержка

Если проект оказался полезен, поставь [звезду](https://github.com/Medium1992/caddy-tblocker/stargazers) и присоединяйся к [Telegram-группе](https://t.me/+96HVPF3Ww6o3YTNi).

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)
