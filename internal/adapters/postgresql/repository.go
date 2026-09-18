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

func (r *Repository) LatestSubscription(ctx context.Context, telegramID int64) (domain.Subscription, bool, error) {
	const query = `
		SELECT id, user_telegram_id, plan_code, status, starts_at, expires_at,
		       quota_bytes, xui_email, created_at, updated_at
		FROM subscriptions
		WHERE user_telegram_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	return r.querySubscription(ctx, query, telegramID)
}

func (r *Repository) ActiveSubscription(ctx context.Context, telegramID int64, now time.Time) (domain.Subscription, bool, error) {
	const query = `
		SELECT id, user_telegram_id, plan_code, status, starts_at, expires_at,
		       quota_bytes, xui_email, created_at, updated_at
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
		&subscription.XUIEmail,
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

func (r *Repository) BindXUIClient(ctx context.Context, subscriptionID int64, email string) error {
	const query = `
		UPDATE subscriptions
		SET xui_email = $2, updated_at = now()
		WHERE id = $1 AND status = 'active'`
	result, err := r.pool.Exec(ctx, query, subscriptionID, email)
	if err != nil {
		return fmt.Errorf("bind 3x-ui client to subscription %d: %w", subscriptionID, err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("bind 3x-ui client: active subscription %d not found", subscriptionID)
	}
	return nil
}

func (r *Repository) CreatePendingPayment(ctx context.Context, request domain.CheckoutRequest, providerPaymentID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

func (r *Repository) CompletePendingPayment(ctx context.Context, telegramID int64, plan domain.Plan, now time.Time) (domain.Subscription, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Subscription{}, false, fmt.Errorf("begin payment confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
			          expires_at, quota_bytes, xui_email, created_at, updated_at`,
			subscription.ID, plan.Code, now, expiresAt, plan.QuotaBytes,
		), &subscription)
	} else {
		err = scanSubscription(tx.QueryRow(ctx, `
			INSERT INTO subscriptions (
				user_telegram_id, plan_code, status, starts_at, expires_at, quota_bytes
			) VALUES ($1, $2, 'active', $3, $4, $5)
			RETURNING id, user_telegram_id, plan_code, status, starts_at,
			          expires_at, quota_bytes, xui_email, created_at, updated_at`,
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

func (r *Repository) HasPendingPayment(ctx context.Context, telegramID int64) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM payments
			WHERE user_telegram_id = $1 AND provider = $2 AND status = 'pending'
		)`, telegramID, betaPaymentProvider,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check pending payment: %w", err)
	}
	return exists, nil
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
			       quota_bytes, xui_email, created_at, updated_at
			FROM subscriptions
			WHERE id = $1 AND user_telegram_id = $2
			FOR UPDATE`, []any{preferredID, telegramID}})
	}
	queries = append(queries, struct {
		query string
		args  []any
	}{`
		SELECT id, user_telegram_id, plan_code, status, starts_at, expires_at,
		       quota_bytes, xui_email, created_at, updated_at
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
		&subscription.XUIEmail,
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
