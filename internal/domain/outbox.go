package domain

type OutboxEvent struct {
	ID         int64
	EventType  string
	TelegramID int64
	Attempts   int
}
