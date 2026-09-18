package domain

import "time"

type Client struct {
	Email         string
	SubID         string
	TelegramID    int64
	Enabled       bool
	QuotaBytes    int64
	UploadBytes   int64
	DownloadBytes int64
	// TrafficUsedBytes is the aggregate returned by modern client endpoints.
	// Upload/Download are kept for adapters that can provide the split counters.
	TrafficUsedBytes int64
	ExpiryAt         time.Time
	InboundIDs       []int64
}

func (c Client) UsedBytes() int64 {
	detailed := c.UploadBytes + c.DownloadBytes
	if c.TrafficUsedBytes > detailed {
		return c.TrafficUsedBytes
	}
	return detailed
}

func (c Client) RemainingBytes() int64 {
	if c.QuotaBytes == 0 {
		return 0
	}
	remaining := c.QuotaBytes - c.UsedBytes()
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (c Client) IsExpired(now time.Time) bool {
	return !c.ExpiryAt.IsZero() && !now.Before(c.ExpiryAt)
}

func (c Client) IsExhausted() bool {
	return c.QuotaBytes > 0 && c.UsedBytes() >= c.QuotaBytes
}

func (c Client) IsActive(now time.Time) bool {
	return c.Enabled && !c.IsExpired(now) && !c.IsExhausted()
}

type Access struct {
	Client Client
	Links  []string
}

type NewClient struct {
	Email      string
	TelegramID int64
	QuotaBytes int64
	ExpiryAt   time.Time
	InboundIDs []int64
	Flow       string
	Comment    string
}
