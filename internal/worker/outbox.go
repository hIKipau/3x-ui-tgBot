package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"x-ui-tgbot/internal/domain"
)

type OutboxRepository interface {
	ClaimOutboxEvent(ctx context.Context) (domain.OutboxEvent, bool, error)
	MarkOutboxProcessed(ctx context.Context, eventID int64) error
	MarkOutboxFailed(ctx context.Context, eventID int64, retryAfter time.Duration, cause error) error
}

type SubscriptionSynchronizer interface {
	SyncSubscription(ctx context.Context, telegramID int64) error
}

type Outbox struct {
	repository   OutboxRepository
	synchronizer SubscriptionSynchronizer
	logger       *slog.Logger
	pollInterval time.Duration
}

func NewOutbox(repository OutboxRepository, synchronizer SubscriptionSynchronizer, logger *slog.Logger) *Outbox {
	return &Outbox{
		repository: repository, synchronizer: synchronizer,
		logger: logger, pollInterval: time.Second,
	}
}

func (w *Outbox) Run(ctx context.Context) error {
	for {
		event, found, err := w.repository.ClaimOutboxEvent(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Error("claim outbox event", "error", err)
			if !wait(ctx, w.pollInterval) {
				return nil
			}
			continue
		}
		if !found {
			if !wait(ctx, w.pollInterval) {
				return nil
			}
			continue
		}

		if err := w.process(ctx, event); err != nil {
			retryAfter := backoff(event.Attempts)
			w.logger.Error("process outbox event", "event_id", event.ID, "event_type", event.EventType, "error", err, "retry_after", retryAfter)
			if markErr := w.repository.MarkOutboxFailed(ctx, event.ID, retryAfter, err); markErr != nil && ctx.Err() == nil {
				w.logger.Error("mark outbox event failed", "event_id", event.ID, "error", markErr)
			}
			continue
		}
		if err := w.repository.MarkOutboxProcessed(ctx, event.ID); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Error("mark outbox event processed", "event_id", event.ID, "error", err)
		}
	}
}

func (w *Outbox) process(ctx context.Context, event domain.OutboxEvent) error {
	if event.EventType != "subscription.activated" {
		return fmt.Errorf("unsupported outbox event type %q", event.EventType)
	}
	if event.TelegramID <= 0 {
		return fmt.Errorf("outbox event has invalid Telegram ID")
	}
	return w.synchronizer.SyncSubscription(ctx, event.TelegramID)
}

func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<(attempt-1)) * 5 * time.Second
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
