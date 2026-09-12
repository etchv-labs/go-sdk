package etchv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAsyncReceipts(t *testing.T) {
	calls := 0
	webhook := "wh_" + strings.Repeat("a", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/async") || r.URL.Query().Get("webhook_id") != webhook || r.Header.Get("Idempotency-Key") != "stable_test_key" {
			t.Error("wrong submission request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"status":"queued","request_id":"req_` + strings.Repeat("b", 64) + `"}`))
	}))
	defer server.Close()
	client, err := NewWithOptions("test-key", server.URL, 1000000000)
	if err != nil {
		t.Fatal(err)
	}
	for _, media := range []string{"images", "documents", "videos"} {
		opts := Options{IdempotencyKey: "stable_test_key"}
		job, err := client.SubmitEmbed(context.Background(), media, []byte("file"), map[string]any{"asset": "test"}, opts, webhook)
		if err != nil || job["status"] != "queued" {
			t.Fatalf("receipt: %v %v", job, err)
		}
		job, err = client.SubmitDetection(context.Background(), media, []byte("file"), opts, webhook)
		if err != nil || job["status"] != "queued" {
			t.Fatalf("receipt: %v %v", job, err)
		}
	}
	if calls != 6 {
		t.Fatalf("unexpected polling: %d requests", calls)
	}
}
