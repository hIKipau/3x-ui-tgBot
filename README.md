# 3x-ui Telegram bot

Telegram-бот для продажи и управления VPN-доступом через 3x-ui. Архитектура
повторяет подход `post-service`: `cmd` → `app` → `transport/usecase/domain` →
`adapters`.

PostgreSQL хранит пользователей, подписки и будущие платежи. 3x-ui хранит
VPN-клиентов и генерирует конфигурационные ссылки. Бот никогда не создаёт
VPN-клиента без активной подписки в PostgreSQL.

## Пользовательский поток

1. `/start` обновляет Telegram-профиль в таблице `users` и показывает последнюю
   подписку, тариф и дату окончания.
2. `/buy` создаёт покупку, `/extend` — продление, после чего бот просит единый
   beta-код из `BETA_PAYMENT_CODE`.
3. Верный код атомарно переводит платёж в `paid` и создаёт либо продлевает
   активную запись в `subscriptions`. Повторно использовать платёж нельзя.
4. `/config` проверяет активную подписку, создаёт клиента 3x-ui при первом
   обращении и возвращает отдельные конфигурации для доступных inbound. Новый клиент получает
   `email=@username` (или `tg-<telegram_id>` без username), комментарий
	   `tgid:<telegram_id>`, flow `xtls-rprx-vision` и безлимитный трафик. При
	   последующих запросах этот внешний идентификатор остаётся стабильным.
5. `/update_config` синхронизирует в 3x-ui лимит и дату окончания из PostgreSQL,
   не меняя UUID/пароль клиента, и получает заново отрендеренные ссылки.

Команды:

- `/start` — регистрация и состояние подписки;
- `/status` — тариф и срок действия;
- `/buy` — покупка подписки;
- `/extend` — продление;
- `/config` — получить VPN-конфигурацию;
- `/update_config` — синхронизировать и получить актуальную конфигурацию;
- `/help` — справка.

## Запуск в Docker

```bash
cp .env.example .env
# заполнить TELEGRAM_BOT_TOKEN, BETA_PAYMENT_CODE, XUI_BASE_URL,
# XUI_API_TOKEN и XUI_INBOUND_IDS

docker compose up --build -d
# или: make docker-up
```

Compose поднимает три сервиса:

- `postgres` — PostgreSQL с постоянным Docker volume;
- `migrator` — однократно применяет все `*.up.sql` перед стартом бота;
- `bot` — собирает и запускает приложение после успешной миграции.

Проверить состояние и посмотреть логи:

```bash
docker compose ps
docker compose logs -f bot
```

Остановка `docker compose down` сохраняет данные. Команда
`docker compose down -v` дополнительно удалит volume с базой.

Для локального запуска Go-приложения без контейнера используется `DATABASE_URL`
из `.env`; PostgreSQL доступен на `127.0.0.1:${POSTGRES_PORT}`:

```bash
go run ./cmd/bot
```

## Миграции

Миграции находятся в `migrations` и используют формат `golang-migrate`:

```bash
make migrate-up       # применить новые миграции
make migrate-down     # откатить одну миграцию
```

Итоговая схема для ручного создания пустой БД находится в
[`database/schema.sql`](database/schema.sql). Инструкция и безопасные команды
полного переноса существующих данных на сервер находятся в
[`database/README.md`](database/README.md). Полный дамп можно создать командой:

```bash
make db-backup
```

API token создаётся в 3x-ui в `Settings -> Security -> API Token`. Он имеет
администраторские права и не должен попадать в git или логи.

Для beta-проверки нажмите `/buy`, затем отправьте значение `BETA_PAYMENT_CODE`
обычным сообщением. Незавершённая покупка хранится в PostgreSQL и не теряется
после перезапуска бота.

При необходимости подписку также можно добавить вручную SQL-запросом:

```sql
INSERT INTO subscriptions (
    user_telegram_id, plan_code, status, starts_at, expires_at, quota_bytes
) VALUES (
    123456789, 'monthly', 'active', now(), now() + interval '30 days',
    0
);
```

Сначала пользователь с этим Telegram ID должен вызвать `/start`.

## Слои

```text
cmd/bot
  -> internal/app
     -> transport/telegram
        -> usecase.Service
           -> Repository port -> adapters/postgresql
           -> Panel port      -> adapters/xui
           -> PaymentAuthorizer -> adapters/payment/beta_code
           -> domain
```

Ключевые таблицы:

- `users` — Telegram-профиль;
- `plans` — тарифы, длительность, лимит трафика и стоимость;
- `vpn_accounts` — постоянная связь Telegram-пользователя с клиентом 3x-ui;
- `subscriptions` — тариф, статус, срок и лимит доступа;
- `payments` — подготовленная схема для провайдера, webhook и идемпотентности;
- `outbox_events` — события для надёжной асинхронной синхронизации и уведомлений.

После подтверждения платежа outbox worker автоматически синхронизирует срок и
лимит клиента с 3x-ui. Ошибки внешнего API повторяются с увеличивающейся задержкой.

## Подключение оплаты

Для подключения реальной оплаты нужно заменить beta-code authorizer адаптером
платёжного провайдера и добавить HTTP webhook transport. Webhook должен в
одной транзакции:

1. заблокировать `payments` по `provider_payment_id` или `idempotency_key`;
2. проигнорировать уже обработанный callback;
3. отметить платёж как `paid`;
4. создать подписку либо продлить `expires_at` существующей;
5. записать событие в outbox;
6. после commit синхронизировать клиента 3x-ui и уведомить пользователя.

## Проверка

```bash
go test ./...
go test -race ./...
go vet ./...
```

Реализация использует современный API 3x-ui 3.x `/panel/api/clients`. Для старых
версий панели можно добавить legacy adapter, не изменяя use cases и Telegram
transport.
