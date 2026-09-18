package domain

import "time"

type SubscriptionStatus string

const (
	SubscriptionPending   SubscriptionStatus = "pending"
	SubscriptionActive    SubscriptionStatus = "active"
	SubscriptionExpired   SubscriptionStatus = "expired"
	SubscriptionCancelled SubscriptionStatus = "cancelled"
)

type Subscription struct {
	ID             int64
	UserTelegramID int64
	PlanCode       string
	Status         SubscriptionStatus
	StartsAt       time.Time
	ExpiresAt      time.Time
	QuotaBytes     int64
	XUIEmail       string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (s Subscription) IsActive(now time.Time) bool {
	return s.Status == SubscriptionActive &&
		(s.StartsAt.IsZero() || !now.Before(s.StartsAt)) &&
		(s.ExpiresAt.IsZero() || now.Before(s.ExpiresAt))
}

type Profile struct {
	User         User
	Subscription *Subscription
}

type Plan struct {
	Code        string
	Name        string
	Duration    time.Duration
	QuotaBytes  int64
	AmountMinor int64
	Currency    string
}

type PurchaseKind string

const (
	PurchaseNew       PurchaseKind = "purchase"
	PurchaseExtension PurchaseKind = "extension"
)

type CheckoutRequest struct {
	User           User
	Plan           Plan
	Kind           PurchaseKind
	SubscriptionID int64
	IdempotencyKey string
}

type Checkout struct {
	ProviderPaymentID string
	URL               string
	RequiresCode      bool
}
