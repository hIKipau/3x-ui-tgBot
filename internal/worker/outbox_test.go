package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"x-ui-tgbot/internal/domain"
)

type repositoryStub struct {
	event     domain.OutboxEvent
	processed int64
	failed    int64
}

func (r *repositoryStub) ClaimOutboxEvent(context.Context) (domain.OutboxEvent, bool, error) {
	return r.event, r.event.ID > 0, nil
}
func (r *repositoryStub) MarkOutboxProcessed(_ context.Context, id int64) error {
	r.processed = id
	return nil
}
func (r *repositoryStub) MarkOutboxFailed(_ context.Context, id int64, _ time.Duration, _ error) error {
	r.failed = id
	return nil
}

type synchronizerStub struct {
	telegramID int64
	err        error
}

func (s *synchronizerStub) SyncSubscription(_ context.Context, telegramID int64) error {
	s.telegramID = telegramID
	return s.err
}

func TestProcessSubscriptionActivated(t *testing.T) {
	repository := &repositoryStub{}
	synchronizer := &synchronizerStub{}
	worker := NewOutbox(repository, synchronizer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	event := domain.OutboxEvent{ID: 1, EventType: "subscription.activated", TelegramID: 42}
	if err := worker.process(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if synchronizer.telegramID != 42 {
		t.Fatalf("telegram ID = %d", synchronizer.telegramID)
	}
}

func TestProcessRejectsUnsupportedEvent(t *testing.T) {
	worker := NewOutbox(&repositoryStub{}, &synchronizerStub{err: errors.New("unused")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := worker.process(context.Background(), domain.OutboxEvent{EventType: "unknown", TelegramID: 42}); err == nil {
		t.Fatal("expected unsupported event error")
	}
}
