package etchv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAccelerator(t *testing.T) {
	job := "req_" + strings.Repeat("b", 64)
	id := strings.Repeat("a", 64)
	png := []byte("\x89PNG\r\n\x1a\nimage")
	var mu sync.Mutex
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "POST" {
			queries = append(queries, r.URL.RawQuery)
		} else if r.URL.RawQuery != "" {
			t.Error("poll requests must not carry a query")
		}
		accel := r.URL.Query().Get("accelerator")
		w.Header().Set("X-Request-ID", job)
		switch r.Method + " " + r.URL.Path {
		case "POST /watermarks/images":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("X-Watermark-ID", id)
			switch accel {
			case "gpu":
				w.Header().Set("X-Etchv-Accelerator", "cpu") // automatic fallback
			case "":
				w.Header().Set("X-Etchv-Accelerator", "quantum") // unknown values are ignored
			}
			_, _ = w.Write(png)
		case "POST /watermarks/images/detect":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Etchv-Accelerator", "gpu")
			_, _ = w.Write([]byte(`{"watermarked":false,"confidence":0.1,"watermark_id":null,"accelerator":"cpu"}`))
		case "POST /watermarks/videos/detect":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"request_id":"` + job + `","status":"queued"}`))
		case "GET /watermarks/detection-jobs/" + job + "/result":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"watermarked":false,"confidence":0.1,"watermark_id":null,"accelerator_requested":"gpu","accelerator":"gpu"}`))
		case "POST /watermarks/documents/async", "POST /watermarks/images/detect/async":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"request_id":"` + job + `","status":"queued","accelerator_requested":"gpu","accelerator":null}`))
		case "GET /watermarks/jobs/" + job:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"` + job + `","status":"succeeded","accelerator_requested":"gpu","accelerator":"cpu"}`))
		default:
			t.Error("unexpected request", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c, _ := New("test-key", WithBaseURL(server.URL), WithTimeout(5*time.Second))
	ctx := context.Background()
	data := map[string]any{"asset": "a"}

	embedded, err := c.EmbedImage(ctx, png, data, Options{Accelerator: AcceleratorGPU})
	if err != nil || embedded.Accelerator != AcceleratorCPU {
		t.Fatal("embed accelerator", embedded, err)
	}
	embedded, err = c.EmbedImage(ctx, png, data, Options{})
	if err != nil || embedded.Accelerator != "" {
		t.Fatal("unknown accelerator must be empty", embedded, err)
	}
	detected, err := c.DetectImage(ctx, png, Options{Accelerator: AcceleratorGPU})
	if err != nil || detected.Accelerator != AcceleratorGPU {
		t.Fatal("header must take precedence", detected, err)
	}
	detected, err = c.DetectVideo(ctx, []byte("video"), Options{Accelerator: AcceleratorGPU})
	if err != nil || detected.Accelerator != AcceleratorGPU {
		t.Fatal("job JSON accelerator", detected, err)
	}
	receipt, err := c.SubmitEmbed(ctx, MediaDocuments, []byte("%PDF-"), data, Options{
		Accelerator: AcceleratorGPU, WebhookID: "wh_" + strings.Repeat("a", 32),
		StorageDestinationID: "dst_" + strings.Repeat("c", 32), StorageKey: "out/file.pdf"})
	if err != nil || receipt.AcceleratorRequested != AcceleratorGPU || receipt.Accelerator != "" {
		t.Fatal("receipt", receipt, err)
	}
	if _, err = c.SubmitDetection(ctx, MediaImages, png, Options{Accelerator: AcceleratorCPU}); err != nil {
		t.Fatal(err)
	}
	status, err := c.GetEmbedJob(ctx, job)
	if err != nil || status.AcceleratorRequested != AcceleratorGPU || status.Accelerator != AcceleratorCPU {
		t.Fatal("job status", status, err)
	}
	for _, bad := range []Accelerator{"GPU", "tpu", " "} {
		if _, err = c.EmbedImage(ctx, png, data, Options{Accelerator: bad}); err == nil || !strings.Contains(err.Error(), "accelerator") {
			t.Fatal("invalid accelerator accepted", bad)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"accelerator=gpu", "", "accelerator=gpu", "accelerator=gpu",
		"accelerator=gpu&storage_destination_id=dst_" + strings.Repeat("c", 32) + "&storage_key=out%2Ffile.pdf&webhook_id=wh_" + strings.Repeat("a", 32),
		"accelerator=cpu"}
	if strings.Join(queries, "|") != strings.Join(want, "|") {
		t.Fatalf("queries:\n got %q\nwant %q", queries, want)
	}
}

func TestRateLimitRetry(t *testing.T) {
	id := strings.Repeat("a", 64)
	var mu sync.Mutex
	var times []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		times = append(times, time.Now())
		if len(times) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0.2")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"detail":{"code":"rate_limited","message":"Too many requests"}}`))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Watermark-ID", id)
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	defer server.Close()
	c, _ := New("test-key", WithBaseURL(server.URL), WithTimeout(5*time.Second))
	if _, err := c.EmbedImage(context.Background(), []byte("png"), map[string]any{"a": 1}, Options{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 2 || times[1].Sub(times[0]) < 150*time.Millisecond {
		t.Fatal("429 must be retried after Retry-After", len(times))
	}
}

func TestRateLimitErrorDetails(t *testing.T) {
	var body, retry string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(429)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	c, _ := New("test-key", WithBaseURL(server.URL))
	check := func(wantCode, wantMessage string, wantLimit int, wantRetry time.Duration, wantText string) {
		t.Helper()
		_, err := c.DetectImage(context.Background(), []byte("png"), Options{})
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
			t.Fatalf("want 429 *Error, got %v", err)
		}
		if apiErr.Code != wantCode || apiErr.Message != wantMessage || apiErr.Limit != wantLimit || apiErr.RetryAfter != wantRetry {
			t.Fatalf("got code %q message %q limit %d retry %v", apiErr.Code, apiErr.Message, apiErr.Limit, apiErr.RetryAfter)
		}
		if got := apiErr.Error(); got != wantText {
			t.Fatalf("Error() = %q, want %q", got, wantText)
		}
	}
	body, retry = `{"detail":{"message":"Too many concurrent jobs","code":"concurrency_limited","limit":2}}`, "7"
	check("concurrency_limited", "Too many concurrent jobs", 2, 7*time.Second, "etchv: HTTP 429: Too many concurrent jobs")
	body, retry = `{"detail":"Too many requests"}`, ""
	check("", "", 0, 0, "etchv: HTTP 429: Too many requests")
	body, retry = `{"detail":{"code":"rate_limited","limit":1.5}}`, "soon"
	check("rate_limited", "", 1, 0, `etchv: HTTP 429: {"code":"rate_limited","limit":1.5}`)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":    0,
		"2.5": 2500 * time.Millisecond,
		"-1":  0,
		now.Add(30 * time.Second).Format(http.TimeFormat): 30 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
	} {
		if got := retryAfter(value, now); got != want {
			t.Fatalf("retryAfter(%q) = %v, want %v", value, got, want)
		}
	}
}
