package xui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"x-ui-tgbot/internal/domain"
)

func TestClientsByTelegramID(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/secret/panel/api/clients/get/tgId/42" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q", got)
		}
		body, err := json.Marshal(map[string]any{
			"success": true,
			"obj": []any{map[string]any{
				"client":      map[string]any{"email": "alice", "tgId": 42, "enable": true, "totalGB": 100},
				"inboundIds":  []int{1, 2},
				"usedTraffic": 30,
			}},
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	})}

	client, err := New("https://panel.example/secret", "secret", httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	clients, err := client.ClientsByTelegramID(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients[0].UsedBytes() != 30 || clients[0].Email != "alice" {
		t.Fatalf("clients = %#v", clients)
	}
}

func TestCreateClientUsesUsernameFlowAndNoLimits(t *testing.T) {
	requests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		var object any
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			client := body["client"].(map[string]any)
			if client["email"] != "@primer" || client["flow"] != "xtls-rprx-vision" ||
				client["comment"] != "tgid:42" || client["totalGB"] != float64(0) ||
				client["limitIp"] != float64(0) {
				t.Fatalf("create client body = %#v", client)
			}
		} else {
			object = map[string]any{"client": map[string]any{
				"email": "@primer", "tgId": 42, "enable": true,
			}}
		}
		body, err := json.Marshal(map[string]any{"success": true, "obj": object})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	})}

	client, err := New("https://panel.example/secret", "secret", httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateClient(context.Background(), domain.NewClient{
		Email: "@primer", TelegramID: 42, Flow: "xtls-rprx-vision",
		Comment: "tgid:42", ExpiryAt: time.Now(), InboundIDs: []int64{2, 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Email != "@primer" || requests != 2 {
		t.Fatalf("client=%#v requests=%d", created, requests)
	}
}

func TestSyncClientAccessNormalizesClientRecord(t *testing.T) {
	requests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		var object any
		switch {
		case r.Method == http.MethodGet && requests == 1:
			object = map[string]any{"client": map[string]any{
				"id": 7, "uuid": "d66f3de1-8cc6-4c3d-b878-2197d961cfa2",
				"email": "alice", "tgId": 42, "enable": true,
				"allowedIPs": `["10.0.0.2/32"]`, "password": "preserved",
				"createdAt": 1, "updatedAt": 2,
			}}
		case r.Method == http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["id"] != "d66f3de1-8cc6-4c3d-b878-2197d961cfa2" || body["password"] != "preserved" {
				t.Fatalf("credentials were not preserved: %#v", body)
			}
			if body["email"] != "@alice" {
				t.Fatalf("email was not renamed: %#v", body)
			}
			if _, exists := body["uuid"]; exists {
				t.Fatalf("database-only uuid field was sent: %#v", body)
			}
			allowed, ok := body["allowedIPs"].([]any)
			if !ok || len(allowed) != 1 || allowed[0] != "10.0.0.2/32" {
				t.Fatalf("allowedIPs = %#v", body["allowedIPs"])
			}
			if body["totalGB"] != float64(100) || body["enable"] != true {
				t.Fatalf("subscription fields = %#v", body)
			}
		case r.Method == http.MethodGet && requests == 3:
			object = map[string]any{"client": map[string]any{
				"email": "@alice", "tgId": 42, "enable": true,
				"totalGB": 100, "expiryTime": time.Unix(0, 0).Add(time.Hour).UnixMilli(),
			}}
		default:
			t.Fatalf("unexpected request %d: %s %s", requests, r.Method, r.URL.Path)
		}

		body, err := json.Marshal(map[string]any{"success": true, "obj": object})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	})}

	client, err := New("https://panel.example/secret", "secret", httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Unix(0, 0).Add(time.Hour)
	got, err := client.SyncClientAccess(context.Background(), "alice", "@alice", 100, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "@alice" || got.QuotaBytes != 100 || requests != 3 {
		t.Fatalf("client=%#v requests=%d", got, requests)
	}
}

func TestAvailableInboundIDsReturnsOnlyEnabledSorted(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/secret/panel/api/inbounds/list/slim" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body, err := json.Marshal(map[string]any{
			"success": true,
			"obj": []any{
				map[string]any{"id": 8, "enable": true},
				map[string]any{"id": 3, "enable": false},
				map[string]any{"id": 2, "enable": true},
			},
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	})}

	client, err := New("https://panel.example/secret", "secret", httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := client.AvailableInboundIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 8 {
		t.Fatalf("inbound IDs = %#v", ids)
	}
}

func TestAttachClientToInbounds(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/secret/panel/api/clients/@alice/attach" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body attachClientRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.InboundIDs) != 2 || body.InboundIDs[0] != 7 || body.InboundIDs[1] != 8 {
			t.Fatalf("body = %#v", body)
		}
		response := []byte(`{"success":true,"obj":null}`)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(response)),
			Request:    r,
		}, nil
	})}

	client, err := New("https://panel.example/secret", "secret", httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AttachClientToInbounds(context.Background(), "@alice", []int64{7, 8}); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
