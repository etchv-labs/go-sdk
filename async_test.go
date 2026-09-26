package etchv

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAsyncReceipts(t *testing.T) {
	calls := 0
	webhook := "wh_" + strings.Repeat("a", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/detect/") && (r.URL.Query().Get("storage_destination_id") != "dst_"+strings.Repeat("c", 32) || r.URL.Query().Get("storage_key") != "a b/#file.pdf") {
			t.Error("storage query was not preserved")
		}
		calls++
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/async") || r.URL.Query().Get("webhook_id") != webhook || r.Header.Get("Idempotency-Key") != "stable_test_key" {
			t.Error("wrong submission request")
		}
		if r.Header.Get("User-Agent") != "etchv-go/"+Version {
			t.Error("missing user agent")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"status":"queued","operation":"embed","request_id":"req_` + strings.Repeat("b", 64) + `","webhook_id":"` + webhook + `","asset_id":null}`))
	}))
	defer server.Close()
	client, err := New("test-key", WithBaseURL(server.URL), WithTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, media := range []Media{MediaImages, MediaDocuments, MediaVideos} {
		opts := Options{IdempotencyKey: "stable_test_key", WebhookID: webhook, StorageDestinationID: "dst_" + strings.Repeat("c", 32), StorageKey: "a b/#file.pdf"}
		job, err := client.SubmitEmbed(context.Background(), media, []byte("file"), map[string]any{"asset": "test"}, opts)
		if err != nil || job.Status != "queued" || job.Done() || *job.WebhookID != webhook || job.AssetID != nil {
			t.Fatalf("receipt: %v %v", job, err)
		}
		opts.StorageDestinationID = ""
		opts.StorageKey = ""
		job, err = client.SubmitDetection(context.Background(), media, []byte("file"), opts)
		if err != nil || job.RequestID != "req_"+strings.Repeat("b", 64) {
			t.Fatalf("receipt: %v %v", job, err)
		}
	}
	if calls != 6 {
		t.Fatalf("unexpected polling: %d requests", calls)
	}
	ctx := context.Background()
	if _, err := client.SubmitEmbed(ctx, "audio", []byte("x"), map[string]any{"a": 1}, Options{}); err == nil {
		t.Fatal("invalid media accepted")
	}
	if _, err := client.EmbedImage(ctx, []byte("x"), map[string]any{"a": 1}, Options{WebhookID: webhook}); err == nil {
		t.Fatal("webhook on a synchronous call accepted")
	}
	if _, err := client.SubmitDetection(ctx, MediaImages, []byte("x"), Options{StorageDestinationID: "dst_" + strings.Repeat("c", 32)}); err == nil {
		t.Fatal("storage on detection accepted")
	}
	if _, err := client.SubmitDetection(ctx, MediaImages, []byte("x"), Options{IdempotencyKey: "bad\r\nkey"}); err == nil {
		t.Fatal("unsafe idempotency key accepted")
	}
	if calls != 6 {
		t.Fatal("invalid input reached the API")
	}
}

func TestJobsAndIdentity(t *testing.T) {
	job := "req_" + strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" || r.Method != "GET" {
			t.Error("wrong request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", job)
		switch r.URL.Path {
		case "/auth/api-key":
			_, _ = w.Write([]byte(`{"organization_id":"org_1","key_id":"key_1","scopes":["watermarks:embed","assets:read"]}`))
		case "/watermarks/jobs/" + job:
			_, _ = w.Write([]byte(`{"request_id":"` + job + `","status":"succeeded","operation":"embed","credits":1,"error_code":null}`))
		case "/watermarks/detection-jobs/" + job:
			_, _ = w.Write([]byte(`{"request_id":"` + job + `","status":"failed","operation":"detect","error_code":"decode_failed"}`))
		case "/watermarks/jobs/" + job + "/result":
			w.WriteHeader(410)
			_, _ = w.Write([]byte(`{"status":"expired","request_id":"` + job + `","detail":"Saved result has expired; this key will not be charged again"}`))
		case "/watermarks/detection-jobs/" + job + "/result":
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"status":"failed","request_id":"` + job + `","error_code":"model_unavailable","detail":"Watermarking failed"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c, _ := New("test-key", WithBaseURL(server.URL), WithTimeout(time.Second))
	ctx := context.Background()
	info, err := c.CheckAPIKey(ctx)
	if err != nil || info.OrganizationID != "org_1" || info.KeyID != "key_1" || len(info.Scopes) != 2 {
		t.Fatal(info, err)
	}
	status, err := c.GetEmbedJob(ctx, job)
	if err != nil || !status.Done() || status.Credits != 1 || status.ErrorCode != nil {
		t.Fatal(status, err)
	}
	status, err = c.GetDetectionJob(ctx, job)
	if err != nil || status.Status != "failed" || *status.ErrorCode != "decode_failed" {
		t.Fatal(status, err)
	}
	_, err = c.GetEmbedResult(ctx, job)
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 410 || e.JobStatus != "expired" || e.RequestID != job || !strings.Contains(e.Error(), "expired") {
		t.Fatal(err)
	}
	_, err = c.GetDetectionResult(ctx, job)
	if !errors.As(err, &e) || e.StatusCode != 503 || e.ErrorCode != "model_unavailable" || e.JobStatus != "failed" {
		t.Fatal("failed jobs must not be retried", err)
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatal("error leaks the API key")
	}
	if _, err = c.GetEmbedJob(ctx, "../jobs"); err == nil {
		t.Fatal("unsafe job ID accepted")
	}
}

func sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhooks(t *testing.T) {
	hook := "wh_" + strings.Repeat("a", 32)
	event := "evt_" + strings.Repeat("e", 64)
	endpoint := map[string]any{"id": hook, "url": "https://example.com/hook", "enabled": true, "created_at": "2026-09-12T00:00:00Z"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reply := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
		switch r.Method + " " + r.URL.Path {
		case "GET /webhooks":
			reply(200, []any{endpoint})
		case "POST /webhooks":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["url"] != "https://example.com/hook" || r.Header.Get("Content-Type") != "application/json" {
				t.Error("wrong create body")
			}
			created := map[string]any{"signing_secret": "whsec_test"}
			for k, v := range endpoint {
				created[k] = v
			}
			reply(201, created)
		case "PATCH /webhooks/" + hook:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["enabled"] != false {
				t.Error("wrong patch")
			}
			reply(200, map[string]any{"id": hook, "enabled": false})
		case "DELETE /webhooks/" + hook:
			w.WriteHeader(204)
		case "GET /webhooks/" + hook + "/deliveries":
			if r.URL.Query().Get("after") != event {
				t.Error("cursor lost")
			}
			reply(200, map[string]any{"data": []any{map[string]any{"id": event, "status": "exhausted", "attempts": 10,
				"history": []any{map[string]any{"at": "2026-09-12T00:00:00Z", "status_code": 500, "error": nil}},
				"payload": map[string]any{"id": event}}}, "next_cursor": nil})
		case "POST /webhooks/" + hook + "/deliveries/" + event + "/redeliver":
			reply(202, map[string]any{"id": event, "status": "queued"})
		default:
			w.Header().Set("X-Request-ID", "req_x")
			reply(409, map[string]any{"detail": "conflict"})
		}
	}))
	defer server.Close()
	c, _ := New("test-key", WithBaseURL(server.URL), WithTimeout(time.Second))
	ctx := context.Background()
	list, err := c.ListWebhooks(ctx)
	if err != nil || len(list) != 1 || list[0].ID != hook || list[0].SigningSecret != "" {
		t.Fatal(list, err)
	}
	created, err := c.CreateWebhook(ctx, "https://example.com/hook")
	if err != nil || created.SigningSecret != "whsec_test" {
		t.Fatal(created, err)
	}
	updated, err := c.UpdateWebhook(ctx, hook, false)
	if err != nil || updated.Enabled {
		t.Fatal(updated, err)
	}
	if err = c.DeleteWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	page, err := c.ListWebhookDeliveries(ctx, hook, event)
	if err != nil || len(page.Data) != 1 || page.NextCursor != nil || *page.Data[0].History[0].StatusCode != 500 || len(page.Data[0].Payload) == 0 {
		t.Fatal(page, err)
	}
	if err = c.RedeliverWebhook(ctx, hook, event); err != nil {
		t.Fatal("202 redelivery must succeed", err)
	}
	if err = c.DeleteWebhook(ctx, "../assets"); err == nil {
		t.Fatal("unsafe webhook ID accepted")
	}
	if err = c.RedeliverWebhook(ctx, hook, "evt_bad"); err == nil {
		t.Fatal("unsafe event ID accepted")
	}

	body := []byte(`{"api_version":"2026-09-12","data":{"request_id":"req_1","status":"succeeded"},"id":"` + event + `","type":"watermark.embed.succeeded"}`)
	now := time.Now().Unix()
	header := http.Header{}
	header.Set("X-Etchv-Event-ID", event)
	header.Set("X-Etchv-Timestamp", strconv.FormatInt(now, 10))
	header.Set("X-Etchv-Signature", sign("whsec_test", now, body))
	ev, err := VerifyWebhook(body, header, "whsec_test")
	if err != nil || ev.Type != "watermark.embed.succeeded" || ev.ID != event {
		t.Fatal(ev, err)
	}
	var job Job
	if json.Unmarshal(ev.Data, &job) != nil || job.Status != "succeeded" {
		t.Fatal("event data does not decode into Job")
	}
	for name, mutate := range map[string]func(h http.Header) ([]byte, string){
		"tampered": func(h http.Header) ([]byte, string) { return append([]byte(" "), body...), "whsec_test" },
		"secret":   func(h http.Header) ([]byte, string) { return body, "whsec_other" },
		"empty":    func(h http.Header) ([]byte, string) { return body, "" },
		"event id": func(h http.Header) ([]byte, string) {
			h.Set("X-Etchv-Event-ID", "evt_other")
			return body, "whsec_test"
		},
		"no sig": func(h http.Header) ([]byte, string) { h.Del("X-Etchv-Signature"); return body, "whsec_test" },
		"stale": func(h http.Header) ([]byte, string) {
			old := now - 301
			h.Set("X-Etchv-Timestamp", strconv.FormatInt(old, 10))
			h.Set("X-Etchv-Signature", sign("whsec_test", old, body))
			return body, "whsec_test"
		},
	} {
		h := header.Clone()
		b, secret := mutate(h)
		if _, err := VerifyWebhook(b, h, secret); !errors.Is(err, ErrInvalidWebhook) {
			t.Fatal("accepted invalid webhook:", name)
		}
	}
}
