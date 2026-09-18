package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const gibibyte int64 = 1024 * 1024 * 1024

type Config struct {
	Env         string
	Timezone    string
	DatabaseURL string
	Telegram    Telegram
	Payment     Payment
	XUI         XUI
	VPN         VPN
}

type Telegram struct {
	Token       string
	PollTimeout time.Duration
}

type Payment struct {
	BetaCode string
}

type XUI struct {
	BaseURL            string
	APIToken           string
	Timeout            time.Duration
	InsecureSkipVerify bool
	TLSCertSHA256      string
}

type VPN struct {
	InboundIDs []int64
	QuotaBytes int64
	Duration   time.Duration
	Flow       string
	PlanCode   string
	PlanName   string
	PriceMinor int64
	Currency   string
}

// Load reads an optional .env file and then the process environment. Process
// environment values always win, which keeps production configuration explicit.
func Load() (*Config, error) {
	fileValues, err := readDotEnv(".env")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read .env: %w", err)
	}

	get := func(key, fallback string) string {
		if value, ok := os.LookupEnv(key); ok {
			return strings.TrimSpace(value)
		}
		if value, ok := fileValues[key]; ok {
			return strings.TrimSpace(value)
		}
		return fallback
	}

	pollTimeout, err := time.ParseDuration(get("TELEGRAM_POLL_TIMEOUT", "25s"))
	if err != nil {
		return nil, fmt.Errorf("parse TELEGRAM_POLL_TIMEOUT: %w", err)
	}
	xuiTimeout, err := time.ParseDuration(get("XUI_TIMEOUT", "10s"))
	if err != nil {
		return nil, fmt.Errorf("parse XUI_TIMEOUT: %w", err)
	}
	duration, err := time.ParseDuration(get("VPN_DEFAULT_DURATION", "720h"))
	if err != nil {
		return nil, fmt.Errorf("parse VPN_DEFAULT_DURATION: %w", err)
	}
	quotaGB, err := strconv.ParseInt(get("VPN_DEFAULT_QUOTA_GB", "0"), 10, 64)
	if err != nil || quotaGB < 0 || quotaGB > (1<<63-1)/gibibyte {
		return nil, fmt.Errorf("VPN_DEFAULT_QUOTA_GB must be a non-negative integer")
	}
	inboundIDs, err := parseIDs(get("XUI_INBOUND_IDS", ""))
	if err != nil {
		return nil, err
	}

	priceMinor, err := strconv.ParseInt(get("VPN_PLAN_PRICE_MINOR", "29900"), 10, 64)
	if err != nil || priceMinor < 0 {
		return nil, fmt.Errorf("VPN_PLAN_PRICE_MINOR must be a non-negative integer")
	}
	insecureSkipVerify, err := strconv.ParseBool(get("XUI_INSECURE_SKIP_VERIFY", "false"))
	if err != nil {
		return nil, fmt.Errorf("parse XUI_INSECURE_SKIP_VERIFY: %w", err)
	}

	cfg := &Config{
		Env:         get("ENV", "local"),
		Timezone:    get("APP_TIMEZONE", "Europe/Moscow"),
		DatabaseURL: get("DATABASE_URL", ""),
		Telegram: Telegram{
			Token:       get("TELEGRAM_BOT_TOKEN", ""),
			PollTimeout: pollTimeout,
		},
		Payment: Payment{
			BetaCode: get("BETA_PAYMENT_CODE", ""),
		},
		XUI: XUI{
			BaseURL:            strings.TrimRight(get("XUI_BASE_URL", ""), "/"),
			APIToken:           get("XUI_API_TOKEN", ""),
			Timeout:            xuiTimeout,
			InsecureSkipVerify: insecureSkipVerify,
			TLSCertSHA256:      strings.ToLower(strings.ReplaceAll(get("XUI_TLS_CERT_SHA256", ""), ":", "")),
		},
		VPN: VPN{
			InboundIDs: inboundIDs,
			QuotaBytes: quotaGB * gibibyte,
			Duration:   duration,
			Flow:       get("VPN_CLIENT_FLOW", "xtls-rprx-vision"),
			PlanCode:   get("VPN_PLAN_CODE", "monthly"),
			PlanName:   get("VPN_PLAN_NAME", "VPN на 30 дней"),
			PriceMinor: priceMinor,
			Currency:   strings.ToUpper(get("VPN_PLAN_CURRENCY", "RUB")),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (cfg *Config) validate() error {
	var missing []string
	if cfg.Telegram.Token == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.Payment.BetaCode == "" {
		missing = append(missing, "BETA_PAYMENT_CODE")
	}
	if cfg.XUI.BaseURL == "" {
		missing = append(missing, "XUI_BASE_URL")
	}
	if cfg.XUI.APIToken == "" {
		missing = append(missing, "XUI_API_TOKEN")
	}
	if len(cfg.VPN.InboundIDs) == 0 {
		missing = append(missing, "XUI_INBOUND_IDS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("required environment variables are missing: %s", strings.Join(missing, ", "))
	}

	u, err := url.ParseRequestURI(cfg.XUI.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("XUI_BASE_URL must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("XUI_BASE_URL must use http or https")
	}
	if cfg.Telegram.PollTimeout <= 0 || cfg.XUI.Timeout <= 0 || cfg.VPN.Duration <= 0 {
		return fmt.Errorf("timeouts and VPN_DEFAULT_DURATION must be positive")
	}
	if cfg.VPN.Duration%(24*time.Hour) != 0 {
		return fmt.Errorf("VPN_DEFAULT_DURATION must contain a whole number of days")
	}
	if cfg.VPN.PlanCode == "" || cfg.VPN.PlanName == "" {
		return fmt.Errorf("VPN plan code and name must not be empty")
	}
	if cfg.VPN.Flow == "" {
		return fmt.Errorf("VPN_CLIENT_FLOW must not be empty")
	}
	if cfg.XUI.InsecureSkipVerify {
		fingerprint, err := hex.DecodeString(cfg.XUI.TLSCertSHA256)
		if err != nil || len(fingerprint) != sha256.Size {
			return fmt.Errorf("XUI_TLS_CERT_SHA256 must contain a SHA-256 certificate fingerprint when XUI_INSECURE_SKIP_VERIFY=true")
		}
	}
	if len(cfg.VPN.Currency) != 3 {
		return fmt.Errorf("VPN_PLAN_CURRENCY must be a three-letter code")
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return fmt.Errorf("load APP_TIMEZONE %q: %w", cfg.Timezone, err)
	}
	return nil
}

func parseIDs(value string) ([]int64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	ids := make([]int64, 0, len(parts))
	seen := make(map[int64]struct{}, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("XUI_INBOUND_IDS must contain positive integers")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func readDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			return nil, fmt.Errorf("empty key in line %q", line)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
