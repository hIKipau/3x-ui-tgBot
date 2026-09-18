package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"x-ui-tgbot/internal/domain"
)

var (
	ErrUserNotFound         = errors.New("user not found")
	ErrSubscriptionRequired = errors.New("active subscription required")
	ErrInvalidPaymentCode   = errors.New("invalid payment code")
	ErrPaymentCodeRateLimit = errors.New("payment code rate limit exceeded")
	ErrNoPendingPayment     = errors.New("pending payment not found")
)

type Repository interface {
	UpsertUser(ctx context.Context, user domain.User) (domain.User, error)
	UserByTelegramID(ctx context.Context, telegramID int64) (domain.User, bool, error)
	LatestSubscription(ctx context.Context, telegramID int64) (domain.Subscription, bool, error)
	ActiveSubscription(ctx context.Context, telegramID int64, now time.Time) (domain.Subscription, bool, error)
	VPNAccountEmail(ctx context.Context, telegramID int64) (string, bool, error)
	BindXUIClient(ctx context.Context, telegramID int64, email string) error
	CreatePendingPayment(ctx context.Context, request domain.CheckoutRequest, providerPaymentID string) error
	CompletePendingPayment(ctx context.Context, telegramID, updateID int64, plan domain.Plan, now time.Time) (domain.Subscription, bool, error)
	PaymentCodeAllowed(ctx context.Context, telegramID int64, now time.Time) (bool, error)
	RecordPaymentCodeFailure(ctx context.Context, telegramID int64, now time.Time) error
	ResetPaymentCodeFailures(ctx context.Context, telegramID int64) error
}

type Panel interface {
	ClientsByTelegramID(ctx context.Context, telegramID int64) ([]domain.Client, error)
	CreateClient(ctx context.Context, client domain.NewClient) (domain.Client, error)
	SyncClientAccess(ctx context.Context, currentEmail, desiredEmail string, quotaBytes int64, expiresAt time.Time) (domain.Client, error)
	ClientLinks(ctx context.Context, email string) ([]string, error)
}

// PaymentAuthorizer is temporary beta authentication for a purchase. A real
// payment provider can later replace it without changing subscription logic.
type PaymentAuthorizer interface {
	Verify(code string) bool
}

type ProvisionPolicy struct {
	InboundIDs []int64
	Flow       string
	Plan       domain.Plan
}

type Service struct {
	repository Repository
	panel      Panel
	payments   PaymentAuthorizer
	policy     ProvisionPolicy
	logger     *slog.Logger
	now        func() time.Time
}

func NewService(repository Repository, panel Panel, payments PaymentAuthorizer, policy ProvisionPolicy, logger *slog.Logger) *Service {
	return &Service{
		repository: repository,
		panel:      panel,
		payments:   payments,
		policy:     policy,
		logger:     logger,
		now:        time.Now,
	}
}

// Start registers or updates the Telegram profile and returns the current
// subscription. Repeated /start calls are safe.
func (s *Service) Start(ctx context.Context, user domain.User) (domain.Profile, error) {
	if user.TelegramID <= 0 {
		return domain.Profile{}, fmt.Errorf("telegram ID must be positive")
	}
	user.Username = sanitizeText(user.Username, 64)
	user.FirstName = sanitizeText(user.FirstName, 128)
	user.LastName = sanitizeText(user.LastName, 128)
	user.LanguageCode = sanitizeText(user.LanguageCode, 16)

	stored, err := s.repository.UpsertUser(ctx, user)
	if err != nil {
		return domain.Profile{}, fmt.Errorf("register Telegram user: %w", err)
	}
	subscription, found, err := s.repository.LatestSubscription(ctx, user.TelegramID)
	if err != nil {
		return domain.Profile{}, fmt.Errorf("read subscription: %w", err)
	}
	profile := domain.Profile{User: stored}
	if found {
		profile.Subscription = &subscription
	}
	return profile, nil
}

func (s *Service) Status(ctx context.Context, telegramID int64) (domain.Profile, error) {
	if telegramID <= 0 {
		return domain.Profile{}, ErrUserNotFound
	}
	subscription, found, err := s.repository.LatestSubscription(ctx, telegramID)
	if err != nil {
		return domain.Profile{}, fmt.Errorf("read subscription: %w", err)
	}
	profile := domain.Profile{User: domain.User{TelegramID: telegramID}}
	if found {
		profile.Subscription = &subscription
	}
	return profile, nil
}

func (s *Service) Buy(ctx context.Context, user domain.User, updateID int64) (domain.Checkout, error) {
	profile, err := s.Start(ctx, user)
	if err != nil {
		return domain.Checkout{}, err
	}
	active, found, err := s.repository.ActiveSubscription(ctx, user.TelegramID, s.now())
	if err != nil {
		return domain.Checkout{}, fmt.Errorf("read subscription: %w", err)
	}
	if found {
		return s.createCheckout(ctx, profile.User, domain.PurchaseExtension, active.ID, updateID)
	}
	return s.createCheckout(ctx, profile.User, domain.PurchaseNew, 0, updateID)
}

func (s *Service) Extend(ctx context.Context, user domain.User, updateID int64) (domain.Checkout, error) {
	profile, err := s.Start(ctx, user)
	if err != nil {
		return domain.Checkout{}, err
	}
	active, found, err := s.repository.ActiveSubscription(ctx, user.TelegramID, s.now())
	if err != nil {
		return domain.Checkout{}, fmt.Errorf("read subscription: %w", err)
	}
	if !found {
		return domain.Checkout{}, ErrSubscriptionRequired
	}
	return s.createCheckout(ctx, profile.User, domain.PurchaseExtension, active.ID, updateID)
}

func (s *Service) createCheckout(ctx context.Context, user domain.User, kind domain.PurchaseKind, subscriptionID, updateID int64) (domain.Checkout, error) {
	paymentID := ""
	if updateID > 0 {
		paymentID = fmt.Sprintf("telegram_%d", updateID)
	} else {
		var err error
		paymentID, err = newPaymentID()
		if err != nil {
			return domain.Checkout{}, fmt.Errorf("generate payment ID: %w", err)
		}
	}
	request := domain.CheckoutRequest{
		User:           user,
		Plan:           s.policy.Plan,
		Kind:           kind,
		SubscriptionID: subscriptionID,
		IdempotencyKey: paymentID,
	}
	if err := s.repository.CreatePendingPayment(ctx, request, paymentID); err != nil {
		return domain.Checkout{}, fmt.Errorf("create pending payment: %w", err)
	}
	return domain.Checkout{ProviderPaymentID: paymentID, RequiresCode: true}, nil
}

// ConfirmPaymentCode atomically consumes the latest pending payment and
// activates or extends the subscription. A consumed payment cannot be reused.
func (s *Service) ConfirmPaymentCode(ctx context.Context, user domain.User, code string, updateID int64) (domain.Subscription, error) {
	if _, err := s.Start(ctx, user); err != nil {
		return domain.Subscription{}, err
	}
	allowed, err := s.repository.PaymentCodeAllowed(ctx, user.TelegramID, s.now())
	if err != nil {
		return domain.Subscription{}, fmt.Errorf("check payment code rate limit: %w", err)
	}
	if !allowed {
		return domain.Subscription{}, ErrPaymentCodeRateLimit
	}
	if !s.payments.Verify(code) {
		if err := s.repository.RecordPaymentCodeFailure(ctx, user.TelegramID, s.now()); err != nil {
			return domain.Subscription{}, fmt.Errorf("record invalid payment code: %w", err)
		}
		return domain.Subscription{}, ErrInvalidPaymentCode
	}
	if err := s.repository.ResetPaymentCodeFailures(ctx, user.TelegramID); err != nil {
		return domain.Subscription{}, fmt.Errorf("reset payment code rate limit: %w", err)
	}
	subscription, found, err := s.repository.CompletePendingPayment(ctx, user.TelegramID, updateID, s.policy.Plan, s.now())
	if err != nil {
		return domain.Subscription{}, fmt.Errorf("complete pending payment: %w", err)
	}
	if !found {
		return domain.Subscription{}, ErrNoPendingPayment
	}
	s.logger.Info("beta payment confirmed", "telegram_id", user.TelegramID, "subscription_id", subscription.ID)
	return subscription, nil
}

// GetConfig checks PostgreSQL first. Only an active subscription may create or
// synchronize a 3x-ui client.
func (s *Service) GetConfig(ctx context.Context, telegramID int64, displayName string) (domain.Access, error) {
	subscription, found, err := s.repository.ActiveSubscription(ctx, telegramID, s.now())
	if err != nil {
		return domain.Access{}, fmt.Errorf("read subscription: %w", err)
	}
	if !found || !subscription.IsActive(s.now()) {
		return domain.Access{}, ErrSubscriptionRequired
	}

	client, err := s.ensurePanelClient(ctx, telegramID, displayName, subscription)
	if err != nil {
		return domain.Access{}, err
	}
	links, err := s.panel.ClientLinks(ctx, client.Email)
	if err != nil {
		return domain.Access{}, fmt.Errorf("get 3x-ui client links: %w", err)
	}
	return domain.Access{Client: client, Links: links}, nil
}

// RefreshConfig synchronizes quota/expiry and asks 3x-ui to render current
// links again. It deliberately does not rotate credentials and break devices.
func (s *Service) RefreshConfig(ctx context.Context, telegramID int64, displayName string) (domain.Access, error) {
	return s.GetConfig(ctx, telegramID, displayName)
}

// SyncSubscription applies committed PostgreSQL access to 3x-ui. The outbox
// worker retries this operation independently from the payment transaction.
func (s *Service) SyncSubscription(ctx context.Context, telegramID int64) error {
	subscription, found, err := s.repository.ActiveSubscription(ctx, telegramID, s.now())
	if err != nil {
		return fmt.Errorf("read active subscription for sync: %w", err)
	}
	if !found {
		return nil
	}
	user, found, err := s.repository.UserByTelegramID(ctx, telegramID)
	if err != nil {
		return fmt.Errorf("read Telegram user for sync: %w", err)
	}
	if !found {
		return ErrUserNotFound
	}
	_, err = s.ensurePanelClient(ctx, telegramID, user.DisplayName(), subscription)
	return err
}

func (s *Service) ensurePanelClient(ctx context.Context, telegramID int64, displayName string, subscription domain.Subscription) (domain.Client, error) {
	accountEmail, _, err := s.repository.VPNAccountEmail(ctx, telegramID)
	if err != nil {
		return domain.Client{}, fmt.Errorf("read VPN account: %w", err)
	}
	clients, err := s.panel.ClientsByTelegramID(ctx, telegramID)
	if err != nil {
		return domain.Client{}, fmt.Errorf("find 3x-ui client: %w", err)
	}

	desiredEmail := clientEmail(telegramID, displayName)
	// The 3x-ui email is an external identity, not a live profile field. Keep
	// the initially assigned nickname stable when the Telegram username changes
	// or is later reassigned to another account.
	if accountEmail != "" {
		desiredEmail = accountEmail
	}
	for _, candidate := range clients {
		if candidate.Email == accountEmail || candidate.Email == desiredEmail || accountEmail == "" {
			client, syncErr := s.panel.SyncClientAccess(ctx, candidate.Email, desiredEmail, subscription.QuotaBytes, subscription.ExpiresAt)
			if syncErr != nil {
				return domain.Client{}, fmt.Errorf("synchronize 3x-ui client: %w", syncErr)
			}
			if accountEmail != client.Email {
				if err := s.repository.BindXUIClient(ctx, telegramID, client.Email); err != nil {
					return domain.Client{}, err
				}
			}
			return client, nil
		}
	}

	client, err := s.panel.CreateClient(ctx, domain.NewClient{
		Email:      desiredEmail,
		TelegramID: telegramID,
		QuotaBytes: subscription.QuotaBytes,
		ExpiryAt:   subscription.ExpiresAt,
		InboundIDs: append([]int64(nil), s.policy.InboundIDs...),
		Flow:       s.policy.Flow,
		Comment:    fmt.Sprintf("tgid:%d", telegramID),
	})
	if err != nil {
		// 3x-ui may partially apply a multi-inbound create. Re-read the
		// deterministic client so a retry remains idempotent.
		clients, lookupErr := s.panel.ClientsByTelegramID(ctx, telegramID)
		if lookupErr != nil {
			return domain.Client{}, fmt.Errorf("provision 3x-ui client: %w", err)
		}
		found := false
		for _, candidate := range clients {
			if candidate.Email == desiredEmail {
				client = candidate
				found = true
				break
			}
		}
		if !found {
			return domain.Client{}, fmt.Errorf("provision 3x-ui client: %w", err)
		}
	}
	if err := s.repository.BindXUIClient(ctx, telegramID, client.Email); err != nil {
		return domain.Client{}, err
	}
	s.logger.Info("3x-ui client provisioned", "telegram_id", telegramID, "email", client.Email)
	return client, nil
}

func clientEmail(telegramID int64, displayName string) string {
	displayName = strings.TrimSpace(displayName)
	if !strings.HasPrefix(displayName, "@") {
		return fmt.Sprintf("tg-%d", telegramID)
	}
	username := strings.TrimPrefix(displayName, "@")
	if username != "" {
		valid := true
		for _, char := range username {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
				(char < '0' || char > '9') && char != '_' {
				valid = false
				break
			}
		}
		if valid {
			return "@" + username
		}
	}
	return fmt.Sprintf("tg-%d", telegramID)
}

func sanitizeText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

func newPaymentID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "beta_" + hex.EncodeToString(value), nil
}
