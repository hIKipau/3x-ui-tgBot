package postgresql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"x-ui-tgbot/internal/domain"
)

func TestBetaPaymentLifecycleIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	database, err := New(ctx, databaseURL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(database)

	telegramID := int64(8_000_000_000_000 + time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() {
		_, _ = database.pool.Exec(ctx, `DELETE FROM outbox_events WHERE payload->>'telegram_id' = $1`, fmt.Sprint(telegramID))
		_, _ = database.pool.Exec(ctx, `DELETE FROM payments WHERE user_telegram_id = $1`, telegramID)
		_, _ = database.pool.Exec(ctx, `DELETE FROM subscriptions WHERE user_telegram_id = $1`, telegramID)
		_, _ = database.pool.Exec(ctx, `DELETE FROM users WHERE telegram_id = $1`, telegramID)
		database.Close()
	})

	user, err := repository.UpsertUser(ctx, domain.User{TelegramID: telegramID, Username: "integration"})
	if err != nil {
		t.Fatal(err)
	}
	plan := domain.Plan{
		Code: "monthly", Duration: 30 * 24 * time.Hour,
		QuotaBytes: 50 * 1024 * 1024 * 1024, AmountMinor: 29900, Currency: "RUB",
	}
	request := domain.CheckoutRequest{
		User: user, Plan: plan, Kind: domain.PurchaseNew, IdempotencyKey: fmt.Sprintf("test-%d-1", telegramID),
	}
	if err := repository.CreatePendingPayment(ctx, request, request.IdempotencyKey); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	subscription, found, err := repository.CompletePendingPayment(ctx, telegramID, plan, now)
	if err != nil || !found {
		t.Fatalf("CompletePendingPayment() found=%v error=%v", found, err)
	}
	if subscription.Status != domain.SubscriptionActive || !subscription.ExpiresAt.Equal(now.Add(plan.Duration)) {
		t.Fatalf("subscription = %#v", subscription)
	}
	if _, found, err := repository.CompletePendingPayment(ctx, telegramID, plan, now); err != nil || found {
		t.Fatalf("payment was reusable: found=%v error=%v", found, err)
	}

	request.Kind = domain.PurchaseExtension
	request.SubscriptionID = subscription.ID
	request.IdempotencyKey = fmt.Sprintf("test-%d-2", telegramID)
	if err := repository.CreatePendingPayment(ctx, request, request.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	extended, found, err := repository.CompletePendingPayment(ctx, telegramID, plan, now)
	if err != nil || !found {
		t.Fatalf("extend payment found=%v error=%v", found, err)
	}
	if !extended.ExpiresAt.Equal(subscription.ExpiresAt.Add(plan.Duration)) {
		t.Fatalf("extended expiry=%v, want %v", extended.ExpiresAt, subscription.ExpiresAt.Add(plan.Duration))
	}
}
