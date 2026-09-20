package xui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"x-ui-tgbot/internal/domain"
)

const maxResponseBytes = 8 << 20

type Client struct {
	baseURL    *url.URL
	apiToken   string
	httpClient *http.Client
	logger     *slog.Logger
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.StatusCode == 0 {
		return "3x-ui API: " + e.Message
	}
	return fmt.Sprintf("3x-ui API returned HTTP %d: %s", e.StatusCode, e.Message)
}

func New(baseURL, apiToken string, httpClient *http.Client, logger *slog.Logger) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("API token is empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{baseURL: u, apiToken: apiToken, httpClient: httpClient, logger: logger}, nil
}

func (c *Client) ClientsByTelegramID(ctx context.Context, telegramID int64) ([]domain.Client, error) {
	var payload []apiClientView
	path := "/panel/api/clients/get/tgId/" + strconv.FormatInt(telegramID, 10)
	if err := c.do(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return nil, err
	}
	clients := make([]domain.Client, 0, len(payload))
	for _, item := range payload {
		clients = append(clients, item.domainClient())
	}
	return clients, nil
}

func (c *Client) CreateClient(ctx context.Context, client domain.NewClient) (domain.Client, error) {
	body := createClientRequest{
		Client: apiClient{
			Email:      client.Email,
			TelegramID: client.TelegramID,
			TotalGB:    client.QuotaBytes,
			ExpiryTime: client.ExpiryAt.UnixMilli(),
			Enable:     true,
			Flow:       client.Flow,
			LimitIP:    0,
			Comment:    client.Comment,
		},
		InboundIDs: client.InboundIDs,
	}
	if err := c.do(ctx, http.MethodPost, "/panel/api/clients/add", body, nil); err != nil {
		return domain.Client{}, err
	}
	return c.ClientByEmail(ctx, client.Email)
}

func (c *Client) ClientByEmail(ctx context.Context, email string) (domain.Client, error) {
	var payload apiClientView
	path := "/panel/api/clients/get/" + url.PathEscape(email)
	if err := c.do(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return domain.Client{}, err
	}
	return payload.domainClient(), nil
}

func (c *Client) ClientLinks(ctx context.Context, email string) ([]string, error) {
	var links []string
	path := "/panel/api/clients/links/" + url.PathEscape(email)
	if err := c.do(ctx, http.MethodGet, path, nil, &links); err != nil {
		return nil, err
	}
	return links, nil
}

// AvailableInboundIDs returns every enabled inbound. Disabled inbounds cannot
// provide a working configuration and are deliberately excluded.
func (c *Client) AvailableInboundIDs(ctx context.Context) ([]int64, error) {
	var payload []apiInbound
	if err := c.do(ctx, http.MethodGet, "/panel/api/inbounds/list/slim", nil, &payload); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(payload))
	for _, inbound := range payload {
		if inbound.ID > 0 && inbound.Enable {
			ids = append(ids, inbound.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// AttachClientToInbounds adds associations without touching the client's
// credentials or removing any existing inbound associations.
func (c *Client) AttachClientToInbounds(ctx context.Context, email string, inboundIDs []int64) error {
	if len(inboundIDs) == 0 {
		return nil
	}
	path := "/panel/api/clients/" + url.PathEscape(email) + "/attach"
	return c.do(ctx, http.MethodPost, path, attachClientRequest{InboundIDs: inboundIDs}, nil)
}

// SyncClientAccess preserves protocol credentials returned by 3x-ui, applies
// subscription fields, and updates the display email when Telegram username
// has changed.
func (c *Client) SyncClientAccess(ctx context.Context, currentEmail, desiredEmail string, quotaBytes int64, expiresAt time.Time) (domain.Client, error) {
	var payload map[string]any
	getPath := "/panel/api/clients/get/" + url.PathEscape(currentEmail)
	if err := c.do(ctx, http.MethodGet, getPath, nil, &payload); err != nil {
		return domain.Client{}, err
	}
	rawClient, ok := payload["client"].(map[string]any)
	if !ok {
		return domain.Client{}, fmt.Errorf("3x-ui client response has no client object")
	}
	if err := normalizeClientForUpdate(rawClient); err != nil {
		return domain.Client{}, fmt.Errorf("normalize 3x-ui client: %w", err)
	}
	if desiredEmail == "" {
		desiredEmail = currentEmail
	}
	rawClient["email"] = desiredEmail
	rawClient["totalGB"] = quotaBytes
	if expiresAt.IsZero() {
		rawClient["expiryTime"] = int64(0)
	} else {
		rawClient["expiryTime"] = expiresAt.UnixMilli()
	}
	rawClient["enable"] = true

	updatePath := "/panel/api/clients/update/" + url.PathEscape(currentEmail)
	if err := c.do(ctx, http.MethodPost, updatePath, rawClient, nil); err != nil {
		return domain.Client{}, err
	}
	return c.ClientByEmail(ctx, desiredEmail)
}

// The read endpoint returns a database ClientRecord, while the update endpoint
// accepts the Xray Client DTO. In particular, the record stores the credential
// in `uuid` and allowed IPs as a JSON string; blindly echoing that response
// makes the panel reject its own payload.
func normalizeClientForUpdate(client map[string]any) error {
	if uuid, ok := client["uuid"].(string); ok && uuid != "" {
		client["id"] = uuid
	} else {
		delete(client, "id")
	}
	delete(client, "uuid")
	delete(client, "createdAt")
	delete(client, "updatedAt")

	value, exists := client["allowedIPs"]
	if !exists || value == nil {
		client["allowedIPs"] = []string{}
		return nil
	}
	if _, ok := value.([]any); ok {
		return nil
	}
	if values, ok := value.([]string); ok {
		client["allowedIPs"] = values
		return nil
	}
	encoded, ok := value.(string)
	if !ok {
		return fmt.Errorf("allowedIPs has unsupported type %T", value)
	}
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		client["allowedIPs"] = []string{}
		return nil
	}
	var allowedIPs []string
	if err := json.Unmarshal([]byte(encoded), &allowedIPs); err == nil {
		client["allowedIPs"] = allowedIPs
		return nil
	}
	for _, item := range strings.Split(encoded, ",") {
		if item = strings.TrimSpace(item); item != "" {
			allowedIPs = append(allowedIPs, item)
		}
	}
	client["allowedIPs"] = allowedIPs
	return nil
}

// DeleteClient and ResetClientTraffic are ready for admin use cases without
// coupling those future workflows to HTTP details.
func (c *Client) DeleteClient(ctx context.Context, email string, keepTraffic bool) error {
	path := "/panel/api/clients/del/" + url.PathEscape(email)
	if keepTraffic {
		path += "?keepTraffic=1"
	}
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

func (c *Client) ResetClientTraffic(ctx context.Context, email string) error {
	path := "/panel/api/clients/resetTraffic/" + url.PathEscape(email)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, result any) error {
	reference, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("parse request path: %w", err)
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(c.baseURL.Path, "/") + reference.Path
	endpoint.RawPath = ""
	endpoint.RawQuery = reference.RawQuery

	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), requestBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Message: responseMessage(data)}
	}

	var envelope apiEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode response envelope: %w", err)
	}
	if !envelope.Success {
		message := envelope.Message
		if message == "" {
			message = "request failed"
		}
		return &APIError{Message: message}
	}
	if result != nil && len(envelope.Object) > 0 && string(envelope.Object) != "null" {
		if err := json.Unmarshal(envelope.Object, result); err != nil {
			return fmt.Errorf("decode response object: %w", err)
		}
	}
	return nil
}

func responseMessage(data []byte) string {
	var envelope apiEnvelope
	if json.Unmarshal(data, &envelope) == nil && envelope.Message != "" {
		return envelope.Message
	}
	message := strings.TrimSpace(string(data))
	if len(message) > 512 {
		message = message[:512]
	}
	if message == "" {
		message = http.StatusText(http.StatusInternalServerError)
	}
	return message
}

type apiEnvelope struct {
	Success bool            `json:"success"`
	Message string          `json:"msg"`
	Object  json.RawMessage `json:"obj"`
}

type createClientRequest struct {
	Client     apiClient `json:"client"`
	InboundIDs []int64   `json:"inboundIds"`
}

type attachClientRequest struct {
	InboundIDs []int64 `json:"inboundIds"`
}

type apiInbound struct {
	ID     int64 `json:"id"`
	Enable bool  `json:"enable"`
}

type apiClientView struct {
	Client      apiClient   `json:"client"`
	InboundIDs  []int64     `json:"inboundIds"`
	Traffic     *apiTraffic `json:"traffic"`
	UsedTraffic int64       `json:"usedTraffic"`
	// Some list endpoints return the client fields at the top level.
	apiClient
}

func (v apiClientView) domainClient() domain.Client {
	client := v.Client
	if client.Email == "" {
		client = v.apiClient
	}
	result := domain.Client{
		Email:            client.Email,
		SubID:            client.SubID,
		TelegramID:       client.TelegramID,
		Enabled:          client.Enable,
		QuotaBytes:       client.TotalGB,
		TrafficUsedBytes: v.UsedTraffic,
		InboundIDs:       append([]int64(nil), v.InboundIDs...),
	}
	if client.ExpiryTime > 0 {
		result.ExpiryAt = time.UnixMilli(client.ExpiryTime)
	}
	if v.Traffic != nil {
		result.UploadBytes = v.Traffic.Up
		result.DownloadBytes = v.Traffic.Down
		if v.Traffic.Enable != nil {
			result.Enabled = *v.Traffic.Enable
		}
	}
	return result
}

type apiClient struct {
	Email      string `json:"email"`
	SubID      string `json:"subId,omitempty"`
	TelegramID int64  `json:"tgId"`
	TotalGB    int64  `json:"totalGB"`
	ExpiryTime int64  `json:"expiryTime"`
	Enable     bool   `json:"enable"`
	Flow       string `json:"flow,omitempty"`
	LimitIP    int    `json:"limitIp"`
	Comment    string `json:"comment,omitempty"`
}

type apiTraffic struct {
	Up     int64 `json:"up"`
	Down   int64 `json:"down"`
	Enable *bool `json:"enable"`
}
