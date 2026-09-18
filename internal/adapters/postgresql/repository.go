package postgresql

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"x-ui-tgbot/internal/domain"
)

const betaPaymentProvider = "beta_code"

const (
	maxPaymentCodeFailures = 5
	paymentCodeBlockPeriod = 15 * time.Minute
)

type Repository struct {
	*PostgreSQL
}

func NewRepository(db *PostgreSQL) *Repository {
	return &Repository{PostgreSQL: db}
}

func (r *Repository) UpsertUser(ctx context.Context, user domain.User) (domain.User, error) {
	const query = `
		INSERT INTO users (telegram_id, username, first_name, last_name, language_code)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (telegram_id) DO UPDATE SET
			username = EXCLUDED.username,
			first_name = EXCLUDED.first_name,
			last_name = EXCLUDED.last_name,
			language_code = EXCLUDED.language_code,
			updated_at = now()
		RETURNING created_at, updated_at`

	err := r.pool.QueryRow(ctx, query,
		user.TelegramID,
		user.Username,
		user.FirstName,
		user.LastName,
		user.LanguageCode,
	).Scan(&user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return domain.User{}, fmt.Errorf("upsert Telegram user %d: %w", user.TelegramID, err)
	}
	return user, nil
}

func (r *Repository) UserByTelegramID(ctx context.Context, telegramID int64) (domain.User, bool, error) {
	var user domain.User
	err := r.pool.QueryRow(ctx, `
		SELECT telegram_id, username, first_name, last_name, language_code, created_at, updated_at
		FROM users WHERE telegram_id = $1`, telegramID,
	).Scan(
		&user.TelegramID, &user.Username, &user.FirstName, &user.LastName,
		&user.LanguageCode, &user.CreatedAt, &user.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, false, nil
	}
	if err != nil {
		return domain.User{}, false, fmt.Errorf("query Telegram user %d: %w", telegramID, err)
	}
	return user, true, nil
}

func (r *Repository) LatestSubscription(ctx context.Context, telegramID int64) (domain.Subscription, bool, error) {
	const query = `
		SELECT id, user_telegram_id, plan_code, status, starts_at, expires_at,
		       quota_bytes, created_at, updated_at
		FROM subscriptions
		WHERE user_telegram_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	return r.querySubscription(ctx, query, telegramID)
}

func (r *Repository) ActiveSubscription(ctx context.Context, telegramID int64, now time.Time) (domain.Subscription, bool, error) {
	const query = `
		SELECT id, user_telegram_id, plan_code, status, starts_at, expires_at,
		       quota_bytes, created_at, updated_at
		FROM subscriptions
		WHERE user_telegram_id = $1
		  AND status = 'active'
		  AND (starts_at IS NULL OR starts_at <= $2)
		  AND (expires_at IS NULL OR expires_at > $2)
		ORDER BY expires_at DESC NULLS FIRST, id DESC
		LIMIT 1`

	return r.querySubscription(ctx, query, telegramID, now)
}

func (r *Repository) querySubscription(ctx context.Context, query string, args ...any) (domain.Subscription, bool, error) {
	var subscription domain.Subscription
	var startsAt, expiresAt *time.Time
	err := r.pool.QueryRow(ctx, query, args...).Scan(
		&subscription.ID,
		&subscription.UserTelegramID,
		&subscription.PlanCode,
		&subscription.Status,
		&startsAt,
		&expiresAt,
		&subscription.QuotaBytes,
		&subscription.CreatedAt,
		&subscription.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Subscription{}, false, nil
	}
	if err != nil {
		return domain.Subscription{}, false, fmt.Errorf("query subscription: %w", err)
	}
	if startsAt != nil {
		subscription.StartsAt = *startsAt
	}
	if expiresAt != nil {
		subscription.ExpiresAt = *expiresAt
	}
	return subscription, true, nil
}

func (r *Repository) VPNAccountEmail(ctx context.Context, telegramID int64) (string, bool, error) {
	var email string
	err := r.pool.QueryRow(ctx, `
		SELECT xui_email
		FROM vpn_accounts
		WHERE user_telegram_id = $1`, telegramID,
	).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query VPN account: %w", err)
	}
	return email, true, nil
}

func (r *Repository) BindXUIClient(ctx context.Context, telegramID int64, email string) error {
	const query = `
		INSERT INTO vpn_accounts (user_telegram_id, xui_email)
		VALUES ($1, $2)
		ON CONFLICT (user_telegram_id) DO UPDATE SET
			xui_email = EXCLUDED.xui_email,
			updated_at = now()`
	_, err := r.pool.Exec(ctx, query, telegramID, email)
	if err != nil {
		return fmt.Errorf("bind 3x-ui client to Telegram user %d: %w", telegramID, err)
	}
	return nil
}

func (r *Repository) CreatePendingPayment(ctx context.Context, request domain.CheckoutRequest, providerPaymentID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUser(ctx, tx, request.User.TelegramID); err != nil {
		return err
	}
	var existingUserID int64
	err = tx.QueryRow(ctx, `
		SELECT user_telegram_id FROM payments WHERE idempotency_key = $1`,
		request.IdempotencyKey,
	).Scan(&existingUserID)
	if err == nil {
		if existingUserID != request.User.TelegramID {
			return fmt.Errorf("payment idempotency key belongs to another user")
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check payment idempotency: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE payments
		SET status = 'cancelled', updated_at = now()
		WHERE user_telegram_id = $1 AND provider = $2 AND status = 'pending'`,
		request.User.TelegramID, betaPaymentProvider,
	); err != nil {
		return fmt.Errorf("cancel previous pending payment: %w", err)
	}

	var subscriptionID any
	if request.SubscriptionID > 0 {
		subscriptionID = request.SubscriptionID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payments (
			user_telegram_id, subscription_id, kind, provider,
			provider_payment_id, idempotency_key, amount_minor, currency, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')`,
		request.User.TelegramID, subscriptionID, request.Kind, betaPaymentProvider,
		providerPaymentID, request.IdempotencyKey, request.Plan.AmountMinor, request.Plan.Currency,
	); err != nil {
		return fmt.Errorf("insert pending payment: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pending payment: %w", err)
	}
	return nil
}

func (r *Repository) CompletePendingPayment(ctx context.Context, telegramID, updateID int64, plan domain.Plan, now time.Time) (domain.Subscription, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Subscription{}, false, fmt.Errorf("begin payment confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUser(ctx, tx, telegramID); err != nil {
		return domain.Subscription{}, false, err
	}
	if updateID > 0 {
		var replayed domain.Subscription
		err = scanSubscription(tx.QueryRow(ctx, `
			SELECT subscription.id, subscription.user_telegram_id, subscription.plan_code,
			       subscription.status, subscription.starts_at, subscription.expires_at,
			       subscription.quota_bytes, subscription.created_at, subscription.updated_at
			FROM telegram_payment_confirmations AS confirmation
			JOIN subscriptions AS subscription ON subscription.id = confirmation.subscription_id
			WHERE confirmation.update_id = $1 AND confirmation.user_telegram_id = $2`,
			updateID, telegramID,
		), &replayed)
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return domain.Subscription{}, false, fmt.Errorf("commit replayed payment lookup: %w", err)
			}
			return replayed, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Subscription{}, false, fmt.Errorf("check payment confirmation idempotency: %w", err)
		}
	}

	var paymentID, preferredSubscriptionID int64
	err = tx.QueryRow(ctx, `
		SELECT id, COALESCE(subscription_id, 0)
		FROM payments
		WHERE user_telegram_id = $1 AND provider = $2 AND status = 'pending'
		ORDER BY created_at DESC, id DESC
		LIMIT 1
		FOR UPDATE`, telegramID, betaPaymentProvider,
	).Scan(&paymentID, &preferredSubscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Subscription{}, false, nil
	}
	if err != nil {
		return domain.Subscription{}, false, fmt.Errorf("lock pending payment: %w", err)
	}

	subscription, found, err := lockSubscription(ctx, tx, telegramID, preferredSubscriptionID, now)
	if err != nil {
		return domain.Subscription{}, false, err
	}
	expiresAt := now.Add(plan.Duration)
	if found {
		base := now
		if subscription.ExpiresAt.After(now) {
			base = subscription.ExpiresAt
		}
		expiresAt = base.Add(plan.Duration)
		err = scanSubscription(tx.QueryRow(ctx, `
			UPDATE subscriptions
			SET plan_code = $2, status = 'active',
			    starts_at = COALESCE(starts_at, $3), expires_at = $4,
			    quota_bytes = $5, updated_at = now()
			WHERE id = $1
			RETURNING id, user_telegram_id, plan_code, status, starts_at,
			          expires_at, quota_bytes, created_at, updated_at`,
			subscription.ID, plan.Code, now, expiresAt, plan.QuotaBytes,
		), &subscription)
	} else {
		err = scanSubscription(tx.QueryRow(ctx, `
			INSERT INTO subscriptions (
				user_telegram_id, plan_code, status, starts_at, expires_at, quota_bytes
			) VALUES ($1, $2, 'active', $3, $4, $5)
			RETURNING id, user_telegram_id, plan_code, status, starts_at,
			          expires_at, quota_bytes, created_at, updated_at`,
			telegramID, plan.Code, now, expiresAt, plan.QuotaBytes,
		), &subscription)
	}
	if err != nil {
		return domain.Subscription{}, false, fmt.Errorf("activate subscription: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE payments
		SET status = 'paid', subscription_id = $2, paid_at = $3, updated_at = now()
		WHERE id = $1`, paymentID, subscription.ID, now,
	); err != nil {
		return domain.Subscription{}, false, fmt.Errorf("mark payment paid: %w", err)
	}
	if updateID > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO telegram_payment_confirmations (update_id, user_telegram_id, subscription_id)
			VALUES ($1, $2, $3)`, updateID, telegramID, subscription.ID,
		); err != nil {
			return domain.Subscription{}, false, fmt.Errorf("record payment confirmation idempotency: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
		VALUES ('subscription', $1, 'subscription.activated',
		        jsonb_build_object(
		            'subscription_id', $2::bigint,
		            'telegram_id', $3::bigint,
		            'payment_id', $4::bigint
		        ))`,
		strconv.FormatInt(subscription.ID, 10), subscription.ID, telegramID, paymentID,
	); err != nil {
		return domain.Subscription{}, false, fmt.Errorf("write subscription outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Subscription{}, false, fmt.Errorf("commit payment confirmation: %w", err)
	}
	return subscription, true, nil
}

func lockSubscription(ctx context.Context, tx pgx.Tx, telegramID, preferredID int64, now time.Time) (domain.Subscription, bool, error) {
	queries := make([]struct {
		query string
		args  []any
	}, 0, 2)
	if preferredID > 0 {
		queries = append(queries, struct {
			query string
			args  []any
		}{`
			SELECT id, user_telegram_id, plan_code, status, starts_at, expires_at,
			       quota_bytes, created_at, updated_at
			FROM subscriptions
			WHERE id = $1 AND user_telegram_id = $2 AND status = 'active'
			FOR UPDATE`, []any{preferredID, telegramID}})
	}
	queries = append(queries, struct {
		query string
		args  []any
	}{`
		SELECT id, user_telegram_id, plan_code, status, starts_at, expires_at,
		       quota_bytes, created_at, updated_at
		FROM subscriptions
		WHERE user_telegram_id = $1 AND status = 'active'
		  AND (expires_at IS NULL OR expires_at > $2)
		ORDER BY expires_at DESC NULLS FIRST, id DESC
		LIMIT 1
		FOR UPDATE`, []any{telegramID, now}})

	for _, candidate := range queries {
		var subscription domain.Subscription
		err := scanSubscription(tx.QueryRow(ctx, candidate.query, candidate.args...), &subscription)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return domain.Subscription{}, false, fmt.Errorf("lock subscription: %w", err)
		}
		return subscription, true, nil
	}
	return domain.Subscription{}, false, nil
}

func scanSubscription(row pgx.Row, subscription *domain.Subscription) error {
	var startsAt, expiresAt *time.Time
	if err := row.Scan(
		&subscription.ID,
		&subscription.UserTelegramID,
		&subscription.PlanCode,
		&subscription.Status,
		&startsAt,
		&expiresAt,
		&subscription.QuotaBytes,
		&subscription.CreatedAt,
		&subscription.UpdatedAt,
	); err != nil {
		return err
	}
	if startsAt != nil {
		subscription.StartsAt = *startsAt
	}
	if expiresAt != nil {
		subscription.ExpiresAt = *expiresAt
	}
	return nil
}

func lockUser(ctx context.Context, tx pgx.Tx, telegramID int64) error {
	var id int64
	if err := tx.QueryRow(ctx, `
		SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, telegramID,
	).Scan(&id); err != nil {
		return fmt.Errorf("lock Telegram user %d: %w", telegramID, err)
	}
	return nil
}

func (r *Repository) UpsertPlan(ctx context.Context, plan domain.Plan) error {
	durationDays := int(plan.Duration / (24 * time.Hour))
	_, err := r.pool.Exec(ctx, `
		INSERT INTO plans (code, name, duration_days, quota_bytes, amount_minor, currency)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (code) DO UPDATE SET
			name = EXCLUDED.name,
			duration_days = EXCLUDED.duration_days,
			quota_bytes = EXCLUDED.quota_bytes,
			amount_minor = EXCLUDED.amount_minor,
			currency = EXCLUDED.currency,
			updated_at = now()`,
		plan.Code, plan.Name, durationDays, plan.QuotaBytes, plan.AmountMinor, plan.Currency,
	)
	if err != nil {
		return fmt.Errorf("upsert plan %q: %w", plan.Code, err)
	}
	return nil
}

func (r *Repository) ClaimOutboxEvent(ctx context.Context) (domain.OutboxEvent, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.OutboxEvent{}, false, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var event domain.OutboxEvent
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id
			FROM outbox_events
			WHERE processed_at IS NULL
			  AND available_at <= now()
			  AND (locked_at IS NULL OR locked_at < now() - interval '5 minutes')
			ORDER BY available_at, created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events AS event
		SET locked_at = now(), attempts = attempts + 1
		FROM candidate
		WHERE event.id = candidate.id
		RETURNING event.id, event.event_type,
		          COALESCE((event.payload->>'telegram_id')::bigint, 0), event.attempts`,
	).Scan(&event.ID, &event.EventType, &event.TelegramID, &event.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OutboxEvent{}, false, nil
	}
	if err != nil {
		return domain.OutboxEvent{}, false, fmt.Errorf("claim outbox event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.OutboxEvent{}, false, fmt.Errorf("commit outbox claim: %w", err)
	}
	return event, true, nil
}

func (r *Repository) MarkOutboxProcessed(ctx context.Context, eventID int64) error {
	result, err := r.pool.Exec(ctx, `
		UPDATE outbox_events
		SET processed_at = now(), locked_at = NULL, last_error = ''
		WHERE id = $1 AND processed_at IS NULL`, eventID,
	)
	if err != nil {
		return fmt.Errorf("mark outbox event %d processed: %w", eventID, err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("outbox event %d is no longer pending", eventID)
	}
	return nil
}

func (r *Repository) MarkOutboxFailed(ctx context.Context, eventID int64, retryAfter time.Duration, cause error) error {
	message := cause.Error()
	runes := []rune(message)
	if len(runes) > 1000 {
		message = string(runes[:1000])
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE outbox_events
		SET locked_at = NULL,
		    available_at = now() + $2::interval,
		    last_error = $3
		WHERE id = $1 AND processed_at IS NULL`,
		eventID, retryAfter.String(), message,
	)
	if err != nil {
		return fmt.Errorf("schedule retry for outbox event %d: %w", eventID, err)
	}
	return nil
}

func (r *Repository) PaymentCodeAllowed(ctx context.Context, telegramID int64, now time.Time) (bool, error) {
	var blockedUntil *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT payment_code_blocked_until FROM users WHERE telegram_id = $1`, telegramID,
	).Scan(&blockedUntil)
	if err != nil {
		return false, fmt.Errorf("read payment code rate limit: %w", err)
	}
	return blockedUntil == nil || !now.Before(*blockedUntil), nil
}

func (r *Repository) RecordPaymentCodeFailure(ctx context.Context, telegramID int64, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment code rate limit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUser(ctx, tx, telegramID); err != nil {
		return err
	}

	var failures int
	var blockedUntil *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT payment_code_failures, payment_code_blocked_until
		FROM users WHERE telegram_id = $1`, telegramID,
	).Scan(&failures, &blockedUntil); err != nil {
		return fmt.Errorf("read payment code failures: %w", err)
	}
	if blockedUntil != nil && !now.Before(*blockedUntil) {
		failures = 0
		blockedUntil = nil
	}
	failures++
	if failures >= maxPaymentCodeFailures {
		until := now.Add(paymentCodeBlockPeriod)
		blockedUntil = &until
		failures = 0
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET payment_code_failures = $2, payment_code_blocked_until = $3, updated_at = now()
		WHERE telegram_id = $1`, telegramID, failures, blockedUntil,
	); err != nil {
		return fmt.Errorf("update payment code failures: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit payment code failure: %w", err)
	}
	return nil
}

func (r *Repository) ResetPaymentCodeFailures(ctx context.Context, telegramID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE users
		SET payment_code_failures = 0, payment_code_blocked_until = NULL, updated_at = now()
		WHERE telegram_id = $1`, telegramID,
	)
	if err != nil {
		return fmt.Errorf("reset payment code failures: %w", err)
	}
	return nil
}
