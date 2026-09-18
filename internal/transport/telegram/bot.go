package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"x-ui-tgbot/internal/domain"
	"x-ui-tgbot/internal/usecase"
)

const telegramMessageLimit = 4000

type AccessService interface {
	Start(ctx context.Context, user domain.User) (domain.Profile, error)
	Status(ctx context.Context, telegramID int64) (domain.Profile, error)
	Buy(ctx context.Context, user domain.User, updateID int64) (domain.Checkout, error)
	Extend(ctx context.Context, user domain.User, updateID int64) (domain.Checkout, error)
	ConfirmPaymentCode(ctx context.Context, user domain.User, code string, updateID int64) (domain.Subscription, error)
	GetConfig(ctx context.Context, telegramID int64, displayName string) (domain.Access, error)
	RefreshConfig(ctx context.Context, telegramID int64, displayName string) (domain.Access, error)
}

type Bot struct {
	baseURL     string
	token       string
	pollTimeout time.Duration
	httpClient  *http.Client
	service     AccessService
	logger      *slog.Logger
	location    *time.Location
}

func New(token string, pollTimeout time.Duration, service AccessService, logger *slog.Logger, location *time.Location) (*Bot, error) {
	if token == "" {
		return nil, fmt.Errorf("Telegram token is empty")
	}
	if service == nil {
		return nil, fmt.Errorf("access service is nil")
	}
	if location == nil {
		return nil, fmt.Errorf("display timezone is nil")
	}
	return &Bot{
		baseURL:     "https://api.telegram.org/bot" + token,
		token:       token,
		pollTimeout: pollTimeout,
		httpClient:  &http.Client{Timeout: pollTimeout + 10*time.Second},
		service:     service,
		logger:      logger,
		location:    location,
	}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	for {
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			b.logger.Error("Telegram polling failed", "error", err)
			if err := waitRetry(ctx, 2*time.Second); err != nil {
				return nil
			}
			continue
		}

		for _, update := range updates {
			offset = update.ID + 1
			if update.Message == nil || update.Message.From == nil {
				continue
			}
			if err := b.handleMessage(ctx, update.ID, *update.Message); err != nil {
				b.logger.Error("handle Telegram message", "error", err, "update_id", update.ID)
			}
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, updateID int64, message message) error {
	// VPN credentials and beta payment codes must never be exposed in a group.
	// Keep this guard even when BotFather group access is disabled: platform
	// settings can be changed independently from a deployment.
	if message.Chat.Type != "private" {
		return nil
	}
	fields := strings.Fields(message.Text)
	if len(fields) == 0 {
		return b.sendText(ctx, message.Chat.ID, "Используйте /help, чтобы увидеть команды.")
	}
	command := strings.ToLower(fields[0])
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}

	switch command {
	case "/start":
		profile, err := b.service.Start(ctx, message.From.domainUser())
		if err != nil {
			return b.replyError(ctx, message.Chat.ID, err)
		}
		return b.sendText(ctx, message.Chat.ID, "Вы зарегистрированы.\n\n"+formatProfile(profile, b.location)+"\n\n"+helpText())
	case "/help":
		return b.sendText(ctx, message.Chat.ID, helpText())
	case "/buy":
		checkout, err := b.service.Buy(ctx, message.From.domainUser(), updateID)
		return b.sendCheckout(ctx, message.Chat.ID, checkout, err)
	case "/extend":
		checkout, err := b.service.Extend(ctx, message.From.domainUser(), updateID)
		if errors.Is(err, usecase.ErrSubscriptionRequired) {
			return b.sendText(ctx, message.Chat.ID, "Активной подписки нет. Используйте /buy.")
		}
		return b.sendCheckout(ctx, message.Chat.ID, checkout, err)
	case "/config":
		access, err := b.service.GetConfig(ctx, message.From.ID, message.From.displayName())
		if errors.Is(err, usecase.ErrSubscriptionRequired) {
			return b.sendText(ctx, message.Chat.ID, "Для получения конфигурации нужна активная подписка. Используйте /buy.")
		}
		if err != nil {
			return b.replyError(ctx, message.Chat.ID, err)
		}
		return b.sendAccess(ctx, message.Chat.ID, access)
	case "/update_config":
		access, err := b.service.RefreshConfig(ctx, message.From.ID, message.From.displayName())
		if errors.Is(err, usecase.ErrSubscriptionRequired) {
			return b.sendText(ctx, message.Chat.ID, "Для обновления конфигурации нужна активная подписка. Используйте /buy.")
		}
		if err != nil {
			return b.replyError(ctx, message.Chat.ID, err)
		}
		if err := b.sendText(ctx, message.Chat.ID, "Конфигурация синхронизирована с подпиской."); err != nil {
			return err
		}
		return b.sendAccess(ctx, message.Chat.ID, access)
	case "/status":
		profile, err := b.service.Status(ctx, message.From.ID)
		if err != nil {
			return b.replyError(ctx, message.Chat.ID, err)
		}
		return b.sendText(ctx, message.Chat.ID, formatProfile(profile, b.location))
	default:
		if strings.HasPrefix(command, "/") {
			return b.sendText(ctx, message.Chat.ID, "Неизвестная команда. Используйте /help.")
		}
		subscription, err := b.service.ConfirmPaymentCode(ctx, message.From.domainUser(), message.Text, updateID)
		if errors.Is(err, usecase.ErrInvalidPaymentCode) {
			return b.sendText(ctx, message.Chat.ID, "Неверный код. Попробуйте ещё раз.")
		}
		if errors.Is(err, usecase.ErrPaymentCodeRateLimit) {
			return b.sendText(ctx, message.Chat.ID, "Слишком много неверных попыток. Повторите через 15 минут.")
		}
		if errors.Is(err, usecase.ErrNoPendingPayment) {
			return b.sendText(ctx, message.Chat.ID, "Нет покупки, ожидающей подтверждения. Используйте /buy.")
		}
		if err != nil {
			return b.replyError(ctx, message.Chat.ID, err)
		}
		return b.sendText(ctx, message.Chat.ID,
			"Код принят. Подписка активна до "+subscription.ExpiresAt.In(b.location).Format("02.01.2006 15:04")+".\nИспользуйте /config, чтобы получить конфигурацию.")
	}
}

func (b *Bot) sendCheckout(ctx context.Context, chatID int64, checkout domain.Checkout, err error) error {
	if err != nil {
		return b.replyError(ctx, chatID, err)
	}
	if checkout.RequiresCode {
		return b.sendText(ctx, chatID, "Требуется код. Отправьте код следующим сообщением.")
	}
	if checkout.URL == "" {
		return b.sendText(ctx, chatID, "Платёж создан, но провайдер не вернул ссылку.")
	}
	return b.sendText(ctx, chatID, "Ссылка для оплаты:\n"+checkout.URL)
}

func (b *Bot) sendAccess(ctx context.Context, chatID int64, access domain.Access) error {
	if err := b.sendText(ctx, chatID, formatStatus(access.Client, b.location)); err != nil {
		return err
	}
	if len(access.Links) == 0 {
		return b.sendText(ctx, chatID, "Доступ создан, но 3x-ui не вернул конфигурации.")
	}
	for index, link := range access.Links {
		name := inboundName(link, access.Client.Email, index)
		message := "<b>Конфиг — " + html.EscapeString(name) +
			" ⬇️</b>\n<pre><code>" + html.EscapeString(link) + "</code></pre>"
		if err := b.sendHTML(ctx, chatID, message); err != nil {
			return err
		}
	}
	return b.sendText(ctx, chatID, "Как подключиться:\n1. Установите V2RayTun или HAPP.\n2. Нажмите на нужный конфиг, скопируйте его и импортируйте в приложение из буфера обмена.")
}

func inboundName(link, clientEmail string, index int) string {
	parsed, err := url.Parse(link)
	if err != nil {
		return fmt.Sprintf("Inbound %d", index+1)
	}
	name := strings.TrimSpace(parsed.Fragment)
	if clientEmail != "" {
		for _, separator := range []string{"-", "_", " "} {
			name = strings.TrimSuffix(name, separator+clientEmail)
		}
	}
	name = strings.TrimRight(name, " -_|/")
	if name == "" {
		return fmt.Sprintf("Inbound %d", index+1)
	}
	return name
}

func (b *Bot) replyError(ctx context.Context, chatID int64, cause error) error {
	b.logger.Error("VPN operation failed", "error", cause, "chat_id", chatID)
	return b.sendText(ctx, chatID, "Не удалось выполнить операцию. Попробуйте позже или обратитесь к администратору.")
}

func (b *Bot) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	seconds := int(b.pollTimeout.Seconds())
	endpoint := b.baseURL + "/getUpdates?timeout=" + strconv.Itoa(seconds) + "&offset=" + strconv.FormatInt(offset, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var response telegramResponse[[]update]
	if err := b.do(req, &response); err != nil {
		return nil, err
	}
	return response.Result, nil
}

func (b *Bot) sendText(ctx context.Context, chatID int64, text string) error {
	for _, chunk := range splitText(text, telegramMessageLimit) {
		body, err := json.Marshal(sendMessageRequest{ChatID: chatID, Text: chunk})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/sendMessage", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		var response telegramResponse[json.RawMessage]
		if err := b.do(req, &response); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) sendHTML(ctx context.Context, chatID int64, text string) error {
	if len([]rune(text)) > telegramMessageLimit {
		return fmt.Errorf("formatted Telegram message exceeds %d characters", telegramMessageLimit)
	}
	body, err := json.Marshal(sendMessageRequest{ChatID: chatID, Text: text, ParseMode: "HTML"})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var response telegramResponse[json.RawMessage]
	return b.do(req, &response)
}

func (b *Bot) do(req *http.Request, target any) error {
	resp, err := b.httpClient.Do(req)
	if err != nil {
		// net/http errors contain the complete request URL. Telegram embeds the
		// bot token in that URL, so returning the raw error would leak it to logs.
		return fmt.Errorf("Telegram API request failed: %s", strings.ReplaceAll(err.Error(), b.token, "[REDACTED]"))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode Telegram response: %w", err)
	}
	var status struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return err
	}
	if !status.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Telegram API: %s", status.Description)
	}
	return nil
}

func formatStatus(client domain.Client, location *time.Location) string {
	status := "активен"
	if !client.Enabled {
		status = "отключён"
	} else if client.IsExpired(time.Now().In(location)) {
		status = "истёк"
	} else if client.IsExhausted() {
		status = "трафик исчерпан"
	}

	lines := []string{"VPN: " + status, "Использовано: " + formatBytes(client.UsedBytes())}
	if client.QuotaBytes == 0 {
		lines = append(lines, "Лимит: без ограничений")
	} else {
		lines = append(lines, "Лимит: "+formatBytes(client.QuotaBytes), "Осталось: "+formatBytes(client.RemainingBytes()))
	}
	if client.ExpiryAt.IsZero() {
		lines = append(lines, "Срок: без ограничений")
	} else {
		lines = append(lines, "Действует до: "+client.ExpiryAt.In(location).Format("02.01.2006 15:04"))
	}
	return strings.Join(lines, "\n")
}

func formatProfile(profile domain.Profile, location *time.Location) string {
	if profile.Subscription == nil {
		return "Подписка: отсутствует\nДля покупки используйте /buy."
	}
	subscription := profile.Subscription
	state := "неактивна"
	now := time.Now().In(location)
	if subscription.IsActive(now) {
		state = "активна"
	} else if subscription.Status == domain.SubscriptionExpired || (!subscription.ExpiresAt.IsZero() && !now.Before(subscription.ExpiresAt)) {
		state = "истекла"
	} else if subscription.Status == domain.SubscriptionPending {
		state = "ожидает оплаты"
	} else if subscription.Status == domain.SubscriptionCancelled {
		state = "отменена"
	}
	lines := []string{"Подписка: " + state, "Тариф: " + subscription.PlanCode}
	if subscription.ExpiresAt.IsZero() {
		lines = append(lines, "Действует: без ограничения срока")
	} else {
		lines = append(lines, "Действует до: "+subscription.ExpiresAt.In(location).Format("02.01.2006 15:04"))
	}
	if subscription.QuotaBytes == 0 {
		lines = append(lines, "Трафик: без ограничений")
	} else {
		lines = append(lines, "Лимит: "+formatBytes(subscription.QuotaBytes))
	}
	return strings.Join(lines, "\n")
}

func helpText() string {
	return "Команды:\n/status — подписка и срок действия\n/buy — купить подписку\n/extend — продлить подписку\n/config — получить конфигурацию\n/update_config — обновить конфигурацию\n/help — справка"
}

func formatBytes(value int64) string {
	const (
		mib = 1024 * 1024
		gib = 1024 * mib
	)
	if value >= gib {
		return fmt.Sprintf("%.2f ГБ", float64(value)/gib)
	}
	return fmt.Sprintf("%.2f МБ", float64(value)/mib)
}

func splitText(value string, limit int) []string {
	runes := []rune(value)
	if len(runes) <= limit {
		return []string{value}
	}
	chunks := make([]string, 0, len(runes)/limit+1)
	for len(runes) > 0 {
		end := min(limit, len(runes))
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}

func waitRetry(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type telegramResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

type update struct {
	ID      int64    `json:"update_id"`
	Message *message `json:"message"`
}

type message struct {
	Text string `json:"text"`
	Chat chat   `json:"chat"`
	From *user  `json:"from"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type user struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	LanguageCode string `json:"language_code"`
}

func (u user) domainUser() domain.User {
	return domain.User{
		TelegramID:   u.ID,
		Username:     u.Username,
		FirstName:    u.FirstName,
		LastName:     u.LastName,
		LanguageCode: u.LanguageCode,
	}
}

func (u user) displayName() string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

type sendMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}
