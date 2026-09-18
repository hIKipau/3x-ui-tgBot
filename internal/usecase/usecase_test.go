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
	user         domain.User
	subscription domain.Subscription
	hasActive    bool
	boundEmail   string
	payment      *domain.CheckoutRequest
	completed    bool
}

func (r *repositoryStub) UpsertUser(_ context.Context, user domain.User) (domain.User, error) {
	r.user = user
	return user, nil
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

func (r *repositoryStub) CreatePendingPayment(_ context.Context, request domain.CheckoutRequest, _ string) error {
	r.payment = &request
	return nil
}

func (r *repositoryStub) HasPendingPayment(context.Context, int64) (bool, error) {
	return r.payment != nil && !r.completed, nil
}

func (r *repositoryStub) CompletePendingPayment(_ context.Context, telegramID int64, plan domain.Plan, now time.Time) (domain.Subscription, bool, error) {
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

	checkout, err := service.Buy(context.Background(), user)
	if err != nil {
		t.Fatalf("Buy() error = %v", err)
	}
	if !checkout.RequiresCode || checkout.ProviderPaymentID == "" || repository.payment == nil {
		t.Fatalf("checkout=%#v payment=%#v", checkout, repository.payment)
	}

	subscription, err := service.ConfirmPaymentCode(context.Background(), user, "valid")
	if err != nil {
		t.Fatalf("ConfirmPaymentCode() error = %v", err)
	}
	if subscription.Status != domain.SubscriptionActive || subscription.ExpiresAt.IsZero() {
		t.Fatalf("subscription = %#v", subscription)
	}
	if _, err := service.ConfirmPaymentCode(context.Background(), user, "valid"); err != ErrNoPendingPayment {
		t.Fatalf("second ConfirmPaymentCode() error = %v", err)
	}
}

func TestConfirmPaymentCodeRejectsInvalidCode(t *testing.T) {
	repository := &repositoryStub{payment: &domain.CheckoutRequest{}}
	service := newTestService(repository, &panelStub{})
	service.payments = paymentStub{valid: false}

	if _, err := service.ConfirmPaymentCode(context.Background(), domain.User{TelegramID: 42}, "invalid"); err != ErrInvalidPaymentCode {
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

func TestGetConfigSynchronizesExistingClient(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	repository := &repositoryStub{hasActive: true, subscription: domain.Subscription{
		ID: 7, Status: domain.SubscriptionActive, ExpiresAt: now.Add(time.Hour), XUIEmail: "alice",
	}}
	panel := &panelStub{clients: []domain.Client{{Email: "alice"}}}
	service := newTestService(repository, panel)

	if _, err := service.RefreshConfig(context.Background(), 42, "Alice"); err != nil {
		t.Fatal(err)
	}
	if !panel.synced || panel.created != nil || panel.syncedFromEmail != "alice" ||
		panel.syncedToEmail != "tg-42" || repository.boundEmail != "tg-42" {
		t.Fatalf("synced=%v from=%q to=%q bound=%q created=%#v",
			panel.synced, panel.syncedFromEmail, panel.syncedToEmail, repository.boundEmail, panel.created)
	}
}
