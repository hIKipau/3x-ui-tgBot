package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"x-ui-tgbot/internal/adapters/payment"
	"x-ui-tgbot/internal/adapters/postgresql"
	"x-ui-tgbot/internal/adapters/xui"
	"x-ui-tgbot/internal/config"
	"x-ui-tgbot/internal/domain"
	telegramtransport "x-ui-tgbot/internal/transport/telegram"
	"x-ui-tgbot/internal/usecase"
	"x-ui-tgbot/internal/worker"
)

// Run is the composition root: concrete adapters are wired to application ports here.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	database, err := postgresql.New(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("initialize PostgreSQL: %w", err)
	}
	defer database.Close()
	repository := postgresql.NewRepository(database)
	plan := domain.Plan{
		Code:        cfg.VPN.PlanCode,
		Name:        cfg.VPN.PlanName,
		Duration:    cfg.VPN.Duration,
		QuotaBytes:  cfg.VPN.QuotaBytes,
		AmountMinor: cfg.VPN.PriceMinor,
		Currency:    cfg.VPN.Currency,
	}
	if err := repository.UpsertPlan(ctx, plan); err != nil {
		return fmt.Errorf("synchronize configured plan: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.XUI.InsecureSkipVerify {
		expectedFingerprint, err := hex.DecodeString(cfg.XUI.TLSCertSHA256)
		if err != nil {
			return fmt.Errorf("decode 3x-ui certificate fingerprint: %w", err)
		}
		transport.TLSClientConfig = &tls.Config{ //nolint:gosec
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return fmt.Errorf("3x-ui TLS peer did not provide a certificate")
				}
				actual := sha256.Sum256(state.PeerCertificates[0].Raw)
				if subtle.ConstantTimeCompare(actual[:], expectedFingerprint) != 1 {
					return fmt.Errorf("3x-ui TLS certificate fingerprint mismatch")
				}
				return nil
			},
		}
		log.Info("3x-ui TLS certificate pinning enabled")
	}
	panelHTTPClient := &http.Client{Timeout: cfg.XUI.Timeout, Transport: transport}
	panel, err := xui.New(cfg.XUI.BaseURL, cfg.XUI.APIToken, panelHTTPClient, log)
	if err != nil {
		return fmt.Errorf("initialize 3x-ui adapter: %w", err)
	}

	service := usecase.NewService(repository, panel, payment.NewBetaCode(cfg.Payment.BetaCode), usecase.ProvisionPolicy{
		InboundIDs: cfg.VPN.InboundIDs,
		Flow:       cfg.VPN.Flow,
		Plan:       plan,
	}, log)

	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("load display timezone: %w", err)
	}
	bot, err := telegramtransport.New(cfg.Telegram.Token, cfg.Telegram.PollTimeout, service, log, location)
	if err != nil {
		return fmt.Errorf("initialize Telegram adapter: %w", err)
	}
	outboxWorker := worker.NewOutbox(repository, service, log)

	log.Info("bot started", "inbound_ids", cfg.VPN.InboundIDs)
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := bot.Run(runCtx); err != nil {
			return fmt.Errorf("run Telegram bot: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		if err := outboxWorker.Run(runCtx); err != nil {
			return fmt.Errorf("run outbox worker: %w", err)
		}
		return nil
	})
	if err := group.Wait(); err != nil {
		return err
	}

	log.Info("bot stopped")
	return nil
}
