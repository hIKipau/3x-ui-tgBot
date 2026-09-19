package usecase

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"x-ui-tgbot/internal/domain"
)

type repositoryStub struct {
	user          domain.User
	subscription  domain.Subscription
	hasActive     bool
	boundEmail    string
	payment       *domain.CheckoutRequest
	completed     bool
	confirmations map[int64]domain.Subscription
}

func (r *repositoryStub) UpsertUser(_ context.Context, user domain.User) (domain.User, error) {
	r.user = user
	return user, nil
}

func (r *repositoryStub) UserByTelegramID(context.Context, int64) (domain.User, bool, error) {
	if r.user.TelegramID == 0 {
		return domain.User{}, false, nil
	}
	return r.user, true, nil
}

func (r *repositoryStub) ActiveSubscription(context.Context, int64, time.Time) (domain.Subscription, bool, error) {
	return r.subscription, r.hasActive, nil
}

func (r *repositoryStub) LatestSubscription(context.Context, int64) (domain.Subscription, bool, error) {
	return r.subscription, r.hasActive, nil
}

func (r *repositoryStub) BindXUIClient(_ context.Context, _ int64, email string) error {
	r.boundEmail = email
	return nil
}

func (r *repositoryStub) VPNAccountEmail(context.Context, int64) (string, bool, error) {
	if r.boundEmail == "" {
		return "", false, nil
	}
	return r.boundEmail, true, nil
}

func (r *repositoryStub) PaymentCodeAllowed(context.Context, int64, time.Time) (bool, error) {
	return true, nil
}

func (r *repositoryStub) RecordPaymentCodeFailure(context.Context, int64, time.Time) error {
	return nil
}

func (r *repositoryStub) ResetPaymentCodeFailures(context.Context, int64) error {
	return nil
}

func (r *repositoryStub) CreatePendingPayment(_ context.Context, request domain.CheckoutRequest, _ string) error {
	r.payment = &request
	return nil
}

func (r *repositoryStub) CompletePendingPayment(_ context.Context, telegramID, updateID int64, plan domain.Plan, now time.Time) (domain.Subscription, bool, error) {
	if subscription, ok := r.confirmations[updateID]; updateID > 0 && ok {
		return subscription, true, nil
	}
	if r.payment == nil || r.completed {
		return domain.Subscription{}, false, nil
	}
	r.completed = true
	base := now
	if r.subscription.ExpiresAt.After(now) {
		base = r.subscription.ExpiresAt
	}
	r.subscription = domain.Subscription{
		ID: 9, UserTelegramID: telegramID, PlanCode: plan.Code,
		Status: domain.SubscriptionActive, StartsAt: now,
		ExpiresAt: base.Add(plan.Duration), QuotaBytes: plan.QuotaBytes,
	}
	r.hasActive = true
	if updateID > 0 {
		if r.confirmations == nil {
			r.confirmations = make(map[int64]domain.Subscription)
		}
		r.confirmations[updateID] = r.subscription
	}
	return r.subscription, true, nil
}

type panelStub struct {
	clients         []domain.Client
	links           []string
	created         *domain.NewClient
	synced          bool
	syncedFromEmail string
	syncedToEmail   string
}

func (p *panelStub) ClientsByTelegramID(context.Context, int64) ([]domain.Client, error) {
	return p.clients, nil
}

func (p *panelStub) CreateClient(_ context.Context, client domain.NewClient) (domain.Client, error) {
	p.created = &client
	return domain.Client{
		Email: client.Email, SubID: "sub-42", TelegramID: client.TelegramID, Enabled: true,
		QuotaBytes: client.QuotaBytes, ExpiryAt: client.ExpiryAt, InboundIDs: client.InboundIDs,
	}, nil
}

func (p *panelStub) SyncClientAccess(_ context.Context, currentEmail, desiredEmail string, quota int64, expiry time.Time) (domain.Client, error) {
	p.synced = true
	p.syncedFromEmail = currentEmail
	p.syncedToEmail = desiredEmail
	return domain.Client{Email: desiredEmail, SubID: "sub-42", Enabled: true, QuotaBytes: quota, ExpiryAt: expiry}, nil
}

func (p *panelStub) ClientLinks(context.Context, string) ([]string, error) {
	if p.links == nil {
		return []string{"vless://example"}, nil
	}
	return p.links, nil
}

type paymentStub struct{ valid bool }

func (p paymentStub) Verify(string) bool {
	return p.valid
}

func newTestService(repository *repositoryStub, panel *panelStub) *Service {
	service := NewService(repository, panel, paymentStub{valid: true}, ProvisionPolicy{
		InboundIDs: []int64{1, 2}, Flow: "xtls-rprx-vision",
		Plan: domain.Plan{Code: "monthly", Duration: 30 * 24 * time.Hour},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.now = func() time.Time { return time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC) }
	return service
}

func TestBuyAndConfirmPaymentCodeActivatesSubscriptionOnce(t *testing.T) {
	repository := &repositoryStub{}
	service := newTestService(repository, &panelStub{})
	user := domain.User{TelegramID: 42, Username: "alice"}

	checkout, err := service.Buy(context.Background(), user, 101)
	if err != nil {
		t.Fatalf("Buy() error = %v", err)
	}
	if !checkout.RequiresCode || checkout.ProviderPaymentID == "" || repository.payment == nil {
		t.Fatalf("checkout=%#v payment=%#v", checkout, repository.payment)
	}

	subscription, err := service.ConfirmPaymentCode(context.Background(), user, "valid", 102)
	if err != nil {
		t.Fatalf("ConfirmPaymentCode() error = %v", err)
	}
	if subscription.Status != domain.SubscriptionActive || subscription.ExpiresAt.IsZero() {
		t.Fatalf("subscription = %#v", subscription)
	}
	replayed, err := service.ConfirmPaymentCode(context.Background(), user, "valid", 102)
	if err != nil || replayed.ID != subscription.ID || !replayed.ExpiresAt.Equal(subscription.ExpiresAt) {
		t.Fatalf("replayed ConfirmPaymentCode() subscription=%#v error=%v", replayed, err)
	}
	if _, err := service.ConfirmPaymentCode(context.Background(), user, "valid", 103); err != ErrNoPendingPayment {
		t.Fatalf("new ConfirmPaymentCode() without pending payment error = %v", err)
	}
}

func TestConfirmPaymentCodeRejectsInvalidCode(t *testing.T) {
	repository := &repositoryStub{payment: &domain.CheckoutRequest{}}
	service := newTestService(repository, &panelStub{})
	service.payments = paymentStub{valid: false}

	if _, err := service.ConfirmPaymentCode(context.Background(), domain.User{TelegramID: 42}, "invalid", 104); err != ErrInvalidPaymentCode {
		t.Fatalf("ConfirmPaymentCode() error = %v", err)
	}
}

func TestStartRegistersUserAndReturnsSubscription(t *testing.T) {
	repository := &repositoryStub{hasActive: true, subscription: domain.Subscription{ID: 7, Status: domain.SubscriptionActive}}
	service := newTestService(repository, &panelStub{})

	profile, err := service.Start(context.Background(), domain.User{TelegramID: 42, Username: "alice"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if repository.user.TelegramID != 42 || profile.Subscription == nil || profile.Subscription.ID != 7 {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestGetConfigRequiresSubscription(t *testing.T) {
	service := newTestService(&repositoryStub{}, &panelStub{})
	_, err := service.GetConfig(context.Background(), 42, "Alice")
	if err != ErrSubscriptionRequired {
		t.Fatalf("GetConfig() error = %v", err)
	}
}

func TestGetConfigCreatesClientForActiveSubscription(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	repository := &repositoryStub{hasActive: true, subscription: domain.Subscription{
		ID: 7, UserTelegramID: 42, Status: domain.SubscriptionActive,
		StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(30 * 24 * time.Hour), QuotaBytes: 0,
	}}
	panel := &panelStub{}
	service := newTestService(repository, panel)

	access, err := service.GetConfig(context.Background(), 42, "@alice")
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if panel.created == nil || panel.created.Email != "@alice" ||
		panel.created.Comment != "tgid:42" || panel.created.Flow != "xtls-rprx-vision" ||
		panel.created.QuotaBytes != 0 {
		t.Fatalf("created client = %#v", panel.created)
	}
	if repository.boundEmail != panel.created.Email || len(access.Links) != 1 || access.Links[0] != "vless://example" {
		t.Fatalf("bound=%q access=%#v", repository.boundEmail, access)
	}
}

func TestClientEmailFallsBackToTelegramIDWithoutUsername(t *testing.T) {
	if got := clientEmail(42, "Alice Smith"); got != "tg-42" {
		t.Fatalf("clientEmail() = %q", got)
	}
	if got := clientEmail(42, "@primer"); got != "@primer" {
		t.Fatalf("clientEmail() = %q", got)
	}
}

func TestGetConfigKeepsExistingVPNAccountIdentity(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	repository := &repositoryStub{hasActive: true, subscription: domain.Subscription{
		ID: 7, Status: domain.SubscriptionActive, ExpiresAt: now.Add(time.Hour),
	}}
	panel := &panelStub{clients: []domain.Client{{Email: "alice"}}}
	repository.boundEmail = "alice"
	service := newTestService(repository, panel)

	if _, err := service.RefreshConfig(context.Background(), 42, "Alice"); err != nil {
		t.Fatal(err)
	}
	if !panel.synced || panel.created != nil || panel.syncedFromEmail != "alice" ||
		panel.syncedToEmail != "alice" || repository.boundEmail != "alice" {
		t.Fatalf("synced=%v from=%q to=%q bound=%q created=%#v",
			panel.synced, panel.syncedFromEmail, panel.syncedToEmail, repository.boundEmail, panel.created)
	}
}

func TestSanitizeTextDoesNotSplitUnicode(t *testing.T) {
	if got := sanitizeText("🙂🙂🙂", 2); got != "🙂🙂" {
		t.Fatalf("sanitizeText() = %q", got)
	}
}
