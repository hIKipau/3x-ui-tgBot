package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"

	"x-ui-tgbot/internal/adapters/payment"
	"x-ui-tgbot/internal/adapters/postgresql"
	"x-ui-tgbot/internal/adapters/xui"
	"x-ui-tgbot/internal/config"
	"x-ui-tgbot/internal/domain"
	telegramtransport "x-ui-tgbot/internal/transport/telegram"
	"x-ui-tgbot/internal/usecase"
)

// Run is the composition root: concrete adapters are wired to application ports here.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	database, err := postgresql.New(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("initialize PostgreSQL: %w", err)
	}
	defer database.Close()
	repository := postgresql.NewRepository(database)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.XUI.InsecureSkipVerify {
		// Explicit opt-in for panels exposed with a self-signed certificate.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		log.Warn("3x-ui TLS certificate verification is disabled")
	}
	panelHTTPClient := &http.Client{Timeout: cfg.XUI.Timeout, Transport: transport}
	panel, err := xui.New(cfg.XUI.BaseURL, cfg.XUI.APIToken, panelHTTPClient, log)
	if err != nil {
		return fmt.Errorf("initialize 3x-ui adapter: %w", err)
	}

	service := usecase.NewService(repository, panel, payment.NewBetaCode(cfg.Payment.BetaCode), usecase.ProvisionPolicy{
		InboundIDs: cfg.VPN.InboundIDs,
		Flow:       cfg.VPN.Flow,
		Plan: domain.Plan{
			Code:        cfg.VPN.PlanCode,
			Name:        cfg.VPN.PlanName,
			Duration:    cfg.VPN.Duration,
			QuotaBytes:  cfg.VPN.QuotaBytes,
			AmountMinor: cfg.VPN.PriceMinor,
			Currency:    cfg.VPN.Currency,
		},
	}, log)

	bot, err := telegramtransport.New(cfg.Telegram.Token, cfg.Telegram.PollTimeout, service, log)
	if err != nil {
		return fmt.Errorf("initialize Telegram adapter: %w", err)
	}

	log.Info("bot started", "inbound_ids", cfg.VPN.InboundIDs)
	if err := bot.Run(ctx); err != nil {
		return fmt.Errorf("run Telegram bot: %w", err)
	}

	log.Info("bot stopped")
	return nil
}
