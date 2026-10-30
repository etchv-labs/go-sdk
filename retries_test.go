package etchv

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var detected = map[string]any{"watermarked": true, "confidence": .99, "watermark_id": strings.Repeat("ab", 32)}

// drop closes the connection without a response, as a reset does.
func drop(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func conflict(w http.ResponseWriter, code string) {
	writeJSON(w, 409, map[string]any{"detail": map[string]any{"code": code, "message": code}})
}

func TestInstantDetectionResendsADroppedConnectionWithTheSameGeneratedKey(t *testing.T) {
	file := []byte("\x89PNG\r\n\x1a\nimage")
	for _, route := range []string{"/watermarks/images/detect", "/watermarks/documents/detect"} {
		var keys []string
		server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.URL.Path != route {
				return false
			}
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			if len(keys) == 1 {
				drop(t, w)
				return true
			}
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, 200, detected)
			return true
		})
		detect := server.client(t).DetectImage
		if strings.Contains(route, "documents") {
			detect = server.client(t).DetectDocument
		}
		result, err := detect(context.Background(), file, Options{})
		if err != nil || !result.Watermarked {
			t.Fatalf("%s: %v %v", route, result, err)
		}
		if len(keys) != 2 || keys[0] != keys[1] || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(keys[0]) {
			t.Fatalf("%s keys: %v", route, keys)
		}
	}
}

func TestInstantDetectionWaitsWhileTheFirstRequestRuns(t *testing.T) {
	var keys []string
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if len(keys) == 1 {
			w.Header().Set("Retry-After", "0.2")
			conflict(w, "request_in_progress")
			return true
		}
		writeJSON(w, 200, detected)
		return true
	})
	start := time.Now()
	if _, err := server.client(t).DetectImage(context.Background(), []byte("image"), Options{IdempotencyKey: "scan-0001"}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("Retry-After not honored: %s", elapsed)
	}
	if strings.Join(keys, ",") != "scan-0001,scan-0001" {
		t.Fatalf("keys: %v", keys)
	}
}

func TestReplayExpiredAndOtherErrorsAreNotRetried(t *testing.T) {
	for _, answer := range []string{"replay_expired", "request_failed", "503"} {
		calls, key := 0, ""
		server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			calls++
			key = r.Header.Get("Idempotency-Key")
			if answer == "503" {
				writeJSON(w, 503, map[string]any{"detail": "busy"})
			} else {
				conflict(w, answer)
			}
			return true
		})
		_, err := server.client(t).DetectImage(context.Background(), []byte("image"), Options{})
		var apiErr *Error
		if !errors.As(err, &apiErr) || calls != 1 || (answer != "503" && (apiErr.StatusCode != 409 || apiErr.Code != answer)) {
			t.Fatalf("%s: %v after %d calls", answer, err, calls)
		}
		if key == "" || apiErr.IdempotencyKey != key {
			t.Fatalf("%s: error key %q, sent %q", answer, apiErr.IdempotencyKey, key)
		}
	}
}

func TestKeyedDetectionOutOfTimeReportsItsKey(t *testing.T) {
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		drop(t, w)
		return true
	})
	_, err := timedClient(t, server, 300*time.Millisecond).DetectImage(context.Background(), []byte("image"), Options{IdempotencyKey: "scan-0002"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 0 || apiErr.IdempotencyKey != "scan-0002" {
		t.Fatalf("deadline: %v", err)
	}
}

func TestKeepResultIsAQueryParameter(t *testing.T) {
	var queries []string
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		queries = append(queries, r.URL.RawQuery)
		writeJSON(w, 200, detected)
		return true
	})
	c := server.client(t)
	keep, discard := true, false
	for _, opts := range []Options{{}, {KeepResult: &discard}, {KeepResult: &keep, Accelerator: AcceleratorGPU}} {
		if _, err := c.DetectImage(context.Background(), []byte("image"), opts); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(queries, "|") != "|keep_result=false|accelerator=gpu&keep_result=true" {
		t.Fatalf("queries: %q", queries)
	}
	if _, err := c.EmbedImage(context.Background(), []byte("image"), map[string]any{"a": 1}, Options{KeepResult: &keep}); err == nil {
		t.Fatal("KeepResult accepted for embedding")
	}
	if _, err := c.DetectVideo(context.Background(), []byte("video"), Options{KeepResult: &keep}); err == nil {
		t.Fatal("KeepResult accepted for video detection")
	}
}

func TestAConsumedUploadIsUploadedAgainWithTheSameKey(t *testing.T) {
	file := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 200))
	var posts []string
	server, calls := uploadServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/watermarks/images/detect" {
			return false
		}
		_ = r.ParseMultipartForm(1 << 20)
		posts = append(posts, r.Header.Get("Idempotency-Key")+" "+r.FormValue("upload_id"))
		switch len(posts) {
		case 1:
			drop(t, w)
		case 2:
			conflict(w, "upload_consumed")
		default:
			writeJSON(w, 200, detected)
		}
		return true
	})
	discard := false
	if _, err := uploadClient(t, server, 100).DetectImage(context.Background(), file, Options{KeepResult: &discard}); err != nil {
		t.Fatal(err)
	}
	uploads := 0
	for _, call := range *calls {
		if call == "POST /uploads" {
			uploads++
		}
	}
	key := strings.Fields(posts[0])[0]
	want := key + " " + testUploadID
	if uploads != 2 || len(posts) != 3 || posts[0] != want || posts[1] != want || posts[2] != want {
		t.Fatalf("uploads %d posts %v", uploads, posts)
	}
}

func TestAnUploadStillHeldByTheFirstCopyIsWaitedFor(t *testing.T) {
	file := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 200))
	var posts []string
	server, calls := uploadServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/watermarks/images/detect" {
			return false
		}
		_ = r.ParseMultipartForm(1 << 20)
		posts = append(posts, r.Header.Get("Idempotency-Key")+" "+r.FormValue("upload_id"))
		switch len(posts) {
		case 1:
			drop(t, w)
		case 2:
			conflict(w, "upload_in_use") // No Retry-After: the claim is taken before the replay check.
		default:
			writeJSON(w, 200, detected)
		}
		return true
	})
	if _, err := uploadClient(t, server, 100).DetectImage(context.Background(), file, Options{}); err != nil {
		t.Fatal(err)
	}
	uploads := 0
	for _, call := range *calls {
		if call == "POST /uploads" {
			uploads++
		}
	}
	key := strings.Fields(posts[0])[0]
	want := key + " " + testUploadID
	if uploads != 1 || len(posts) != 3 || posts[0] != want || posts[1] != want || posts[2] != want {
		t.Fatalf("uploads %d posts %v", uploads, posts)
	}
}

func TestUploadInUseIsNotWaitedForOnAFileInTheBody(t *testing.T) {
	calls := 0
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		calls++
		conflict(w, "upload_in_use")
		return true
	})
	_, err := server.client(t).DetectImage(context.Background(), []byte("image"), Options{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "upload_in_use" || calls != 1 ||
		!regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(apiErr.IdempotencyKey) {
		t.Fatalf("%v after %d calls", err, calls)
	}
}

func TestTheResendAfterANewUploadKeepsTheFirstDeadline(t *testing.T) {
	file := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 200))
	var posts, sessions, puts atomic.Int32
	server, _ := uploadServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/uploads":
			// The re-upload is slow: its session and PUT together outlast the 2 s timeout, each
			// well within it. A slow runner only adds time, so it can only reach the deadline sooner.
			if sessions.Add(1) == 2 {
				time.Sleep(1200 * time.Millisecond)
			}
			return false
		case strings.HasPrefix(r.URL.Path, "/signed/"):
			if puts.Add(1) == 2 {
				time.Sleep(1200 * time.Millisecond)
			}
			return false
		case r.URL.Path != "/watermarks/images/detect":
			return false
		}
		if posts.Add(1) == 1 {
			conflict(w, "upload_consumed")
			return true
		}
		// Reached only with a fresh timeout window per send, which is the regression this catches.
		writeJSON(w, 200, detected)
		return true
	})
	c, err := New("test-key", WithBaseURL(server.URL), WithHTTPClient(server.Client()), WithTimeout(2*time.Second), WithLargeFileThreshold(100))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DetectImage(context.Background(), file, Options{IdempotencyKey: "scan-0004"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 0 || apiErr.IdempotencyKey != "scan-0004" ||
		posts.Load() != 1 || sessions.Load() != 2 || puts.Load() != 2 {
		t.Fatalf("%v after %d posts, %d sessions, %d puts", err, posts.Load(), sessions.Load(), puts.Load())
	}
}

func TestADownloadOutOfTimeReturnsTheNetworkError(t *testing.T) {
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		drop(t, w)
		return true
	})
	_, err := timedClient(t, server, 1500*time.Millisecond).DownloadAsset(context.Background(), "ast_"+strings.Repeat("a", 64))
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 0 || strings.Contains(apiErr.Detail, "job may still complete") ||
		errors.Is(err, context.DeadlineExceeded) || errors.Unwrap(err) == nil {
		t.Fatalf("download: %v", err)
	}
}

func TestDownloadsResendADroppedConnection(t *testing.T) {
	asset, delivery := "ast_"+strings.Repeat("a", 64), "std_"+strings.Repeat("d", 64)
	var mu sync.Mutex
	calls := map[string]int{}
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		mu.Lock()
		calls[r.URL.Path]++
		first := calls[r.URL.Path] == 1
		mu.Unlock()
		if first {
			drop(t, w)
			return true
		}
		_, _ = w.Write([]byte("file"))
		return true
	})
	c := server.client(t)
	a, err := c.DownloadAsset(context.Background(), asset)
	if err != nil || string(a.Bytes) != "file" {
		t.Fatalf("asset: %v", err)
	}
	d, err := c.DownloadStorageDelivery(context.Background(), delivery)
	if err != nil || string(d.Bytes) != "file" {
		t.Fatalf("delivery: %v", err)
	}
	if calls["/assets/"+asset+"/content"] != 2 || calls["/storage/deliveries/"+delivery+"/content"] != 2 {
		t.Fatalf("calls: %v", calls)
	}
}

func TestDownloadErrorsAreNotRetried(t *testing.T) {
	calls := 0
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		calls++
		writeJSON(w, 503, map[string]any{"detail": "busy"})
		return true
	})
	_, err := server.client(t).DownloadAsset(context.Background(), "ast_"+strings.Repeat("a", 64))
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 || calls != 1 {
		t.Fatalf("%v after %d calls", err, calls)
	}
}
