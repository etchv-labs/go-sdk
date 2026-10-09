package etchv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testBatchID   = "bat_0123456789abcdef0123456789abcdef"
	testRequestID = "req_" + "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// batchServer is a fake API plus signed upload host. handle may answer a
// request first; calls records "METHOD path" in order.
type batchServer struct {
	*httptest.Server
	t     *testing.T
	mu    sync.Mutex
	calls []string
	keys  []string
	puts  map[string]string
}

func newBatchServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body []byte) bool) *batchServer {
	t.Helper()
	s := &batchServer{t: t, puts: map[string]string{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/signed/") {
			if r.Header.Get("X-API-Key") != "" || r.Header.Get("Authorization") != "" {
				t.Error("credentials sent to the signed upload URL")
			}
			partial := r.URL.Path == "/signed/stuck" || r.URL.Path == "/signed/short"
			if r.Method != "PUT" || r.URL.Query().Get("Signature") != "s" || (!partial && r.ContentLength != int64(len(body))) {
				t.Errorf("bad upload %s %s length %d", r.Method, r.URL, r.ContentLength)
			}
			if r.URL.Path == "/signed/refused" {
				w.WriteHeader(403)
				return
			}
			s.mu.Lock()
			s.puts[strings.TrimPrefix(r.URL.Path, "/signed/")] = string(body)
			s.mu.Unlock()
			w.WriteHeader(200)
			return
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("missing API key on %s %s", r.Method, r.URL.Path)
		}
		if handle(w, r, body) {
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *batchServer) client(t *testing.T) *Client {
	t.Helper()
	c, err := New("test-key", WithBaseURL(s.URL), WithHTTPClient(s.Client()), WithTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *batchServer) callList() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.calls, ",")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func batchView(status string, items ...map[string]any) map[string]any {
	return map[string]any{"batch_id": testBatchID, "status": status, "item_count": len(items), "archive": false, "accelerator": "cpu",
		"webhook_id": nil, "storage_destination_id": nil,
		"counts":  map[string]int{"pending": 0, "accepted": 0, "rejected": 0, "succeeded": 0, "failed": 0, "in_progress": 0},
		"credits": map[string]int{"reserved": 0, "charged": 0, "refunded": 0}, "cancel_requested": false,
		"created_at": "2026-10-09T00:00:00+00:00", "started_at": nil, "completed_at": nil,
		"upload_expires_at": "2026-10-10T00:00:00+00:00", "status_url": "/watermarks/batches/" + testBatchID, "items": items}
}

func TestSubmitBatchCreatesUploadsWithoutTheKeyAndStarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "contract-bolt.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.7 bolt"), 0o600); err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	var server *batchServer
	server = newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch r.Method + " " + r.URL.Path {
		case "POST /watermarks/batches":
			server.keys = append(server.keys, r.Header.Get("Idempotency-Key"))
			_ = json.Unmarshal(body, &created)
			items := []map[string]any{}
			for i, name := range []string{"logo.png", "contract-bolt.pdf"} {
				items = append(items, map[string]any{"index": i, "filename": name, "status": "pending",
					"upload": map[string]any{"method": "PUT", "url": server.URL + "/signed/" + name + "?Signature=s", "expires_at": "x"}})
			}
			writeJSON(w, 201, batchView("draft", items...))
		case "POST /watermarks/batches/" + testBatchID + "/start":
			w.Header().Set("Retry-After", "2")
			writeJSON(w, 202, batchView("starting",
				map[string]any{"index": 0, "filename": "logo.png", "status": "pending"},
				map[string]any{"index": 1, "filename": "contract-bolt.pdf", "status": "pending"}))
		default:
			return false
		}
		return true
	})
	batch, err := server.client(t).SubmitBatch(context.Background(), []BatchItem{
		{Filename: "logo.png", File: []byte("\x89PNG logo"), Data: map[string]any{"recipient": "logo"}},
		{Path: path, Data: map[string]any{"recipient": "bolt"}},
	}, &BatchOptions{Archive: true, UploadConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if batch.BatchID != testBatchID || batch.Status != "starting" || len(batch.Items) != 2 {
		t.Fatalf("batch: %+v", batch)
	}
	if len(server.keys) != 1 || !idempotencyKey.MatchString(server.keys[0]) {
		t.Fatalf("idempotency keys: %v", server.keys)
	}
	items := created["items"].([]any)
	second := items[1].(map[string]any)
	if created["archive"] != true || len(items) != 2 || second["filename"] != "contract-bolt.pdf" || second["size"] != float64(13) ||
		second["data"].(map[string]any)["recipient"] != "bolt" {
		t.Fatalf("create body: %v", created)
	}
	if server.puts["logo.png"] != "\x89PNG logo" || server.puts["contract-bolt.pdf"] != "%PDF-1.7 bolt" {
		t.Fatalf("uploads: %q", server.puts)
	}
	calls := server.callList()
	if !strings.HasPrefix(calls, "POST /watermarks/batches,PUT /signed/") || !strings.HasSuffix(calls, "POST /watermarks/batches/"+testBatchID+"/start") ||
		strings.Count(calls, "PUT ") != 2 {
		t.Fatalf("calls: %s", calls)
	}
}

func TestSubmitBatchRetriesWithTheSameKeyButNot503(t *testing.T) {
	attempts := 0
	var server *batchServer
	server = newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch r.URL.Path {
		case "/watermarks/batches":
			attempts++
			server.keys = append(server.keys, r.Header.Get("Idempotency-Key"))
			if attempts == 1 {
				w.Header().Set("Retry-After", "0.01")
				writeJSON(w, 429, map[string]any{"detail": map[string]any{"code": "rate_limited", "message": "slow down"}})
				return true
			}
			// A replay: a.png already arrived, b.png still needs its upload.
			writeJSON(w, 200, batchView("draft",
				map[string]any{"index": 0, "filename": "a.png", "status": "pending", "upload_received": true},
				map[string]any{"index": 1, "filename": "b.png", "status": "pending", "upload_received": nil,
					"upload": map[string]any{"method": "PUT", "url": server.URL + "/signed/b.png?Signature=s", "expires_at": "x"}}))
		case "/watermarks/batches/" + testBatchID + "/start":
			writeJSON(w, 200, batchView("processing"))
		default:
			return false
		}
		return true
	})
	_, err := server.client(t).SubmitBatch(context.Background(), []BatchItem{
		{Filename: "a.png", File: []byte("x"), Data: map[string]any{"a": 1}},
		{Filename: "b.png", File: []byte("y"), Data: map[string]any{"b": 1}},
	}, &BatchOptions{IdempotencyKey: "nightly-2026-10-09"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(server.keys, ",") != "nightly-2026-10-09,nightly-2026-10-09" {
		t.Fatalf("keys: %v", server.keys)
	}
	if calls := server.callList(); strings.Count(calls, "PUT ") != 1 || !strings.Contains(calls, "PUT /signed/b.png") || server.puts["b.png"] != "y" {
		t.Fatalf("only the file not yet received should be uploaded: %s", calls)
	}

	unavailable := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		writeJSON(w, 503, map[string]any{"detail": "Batch uploads are unavailable; send one zip to /watermarks/batches/zip instead"})
		return true
	})
	_, err = unavailable.client(t).SubmitBatch(context.Background(), []BatchItem{{Filename: "a.png", File: []byte("x"), Data: map[string]any{"a": 1}}}, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 || !strings.Contains(apiErr.Detail, "zip") || unavailable.callList() != "POST /watermarks/batches" {
		t.Fatalf("503: %v calls %s", err, unavailable.callList())
	}
}

func TestSubmitBatchReportsAFailedUpload(t *testing.T) {
	var server *batchServer
	server = newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.URL.Path != "/watermarks/batches" {
			return false
		}
		writeJSON(w, 201, batchView("draft", map[string]any{"index": 0, "filename": "a.png", "status": "pending",
			"upload": map[string]any{"method": "PUT", "url": server.URL + "/signed/refused?Signature=s", "expires_at": "x"}}))
		return true
	})
	_, err := server.client(t).SubmitBatch(context.Background(), []BatchItem{{Filename: "a.png", File: []byte("x"), Data: map[string]any{"a": 1}}},
		&BatchOptions{IdempotencyKey: "resume-me-01"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || apiErr.IdempotencyKey != "resume-me-01" || !strings.Contains(apiErr.Detail, testBatchID) {
		t.Fatalf("error: %v", err)
	}
	if strings.Contains(server.callList(), "/start") {
		t.Fatal("started a batch with a failed upload")
	}
}

func TestBatchLimitsAreCheckedBeforeAnyRequest(t *testing.T) {
	server := newBatchServer(t, func(http.ResponseWriter, *http.Request, []byte) bool { return false })
	c := server.client(t)
	items := make([]BatchItem, MaxBatchItems+1)
	for i := range items {
		items[i] = BatchItem{Filename: "a.png", File: []byte("x"), Data: map[string]any{"a": 1}}
	}
	if _, err := c.SubmitBatch(context.Background(), items, nil); err == nil || !strings.Contains(err.Error(), "1 to 100") {
		t.Fatalf("101 items: %v", err)
	}
	if _, err := c.SubmitBatch(context.Background(), nil, nil); err == nil {
		t.Fatal("empty batch accepted")
	}
	if _, err := c.SubmitBatch(context.Background(), items[:1], &BatchOptions{IdempotencyKey: "short"}); err == nil {
		t.Fatal("invalid idempotency key accepted")
	}
	if _, err := c.SubmitBatch(context.Background(), []BatchItem{{Filename: "a.png", File: []byte("x")}}, nil); err == nil {
		t.Fatal("missing data accepted")
	}
	if _, err := c.GetBatch(context.Background(), "bat_nope"); err == nil {
		t.Fatal("invalid batch ID accepted")
	}
	if _, err := c.ListBatches(context.Background(), 51, ""); err == nil {
		t.Fatal("limit 51 accepted")
	}
	if calls := server.callList(); calls != "" {
		t.Fatalf("requests sent: %s", calls)
	}
}

func TestWaitForBatchHonorsRetryAfter(t *testing.T) {
	polls := 0
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.URL.Path != "/watermarks/batches/"+testBatchID {
			return false
		}
		polls++
		if polls == 1 {
			w.Header().Set("Retry-After", "0.4")
			writeJSON(w, 200, batchView("processing"))
			return true
		}
		writeJSON(w, 200, batchView("completed"))
		return true
	})
	start := time.Now()
	batch, err := server.client(t).WaitForBatch(context.Background(), testBatchID, time.Minute)
	if err != nil || batch.Status != "completed" || !batch.Done() {
		t.Fatalf("wait: %+v %v", batch, err)
	}
	if elapsed := time.Since(start); polls != 2 || elapsed < 400*time.Millisecond {
		t.Fatalf("polls %d after %s", polls, elapsed)
	}

	slow := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		w.Header().Set("Retry-After", "0.05")
		writeJSON(w, 200, batchView("processing"))
		return true
	})
	_, err = slow.client(t).WaitForBatch(context.Background(), testBatchID, 120*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "processing") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestBatchResultsWaitsThenSurfacesEachFile(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nwatermarked")
	polls := 0
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch r.URL.Path {
		case "/watermarks/batches/" + testBatchID:
			if polls++; polls == 1 {
				w.Header().Set("Retry-After", "0.05")
				writeJSON(w, 200, batchView("processing"))
				return true
			}
			writeJSON(w, 200, batchView("completed",
				map[string]any{"index": 0, "filename": "logo.png", "status": "succeeded", "request_id": testRequestID, "credits": 1,
					"result_url": "/watermarks/jobs/" + testRequestID + "/result"},
				map[string]any{"index": 1, "filename": "contract.pdf", "status": "rejected", "error_code": "upload_size_mismatch",
					"error_detail": "The uploaded file is not the declared size", "credits": nil}))
		case "/watermarks/jobs/" + testRequestID + "/result":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("X-Watermark-ID", strings.Repeat("ab", 32))
			w.Header().Set("X-Request-ID", testRequestID)
			_, _ = w.Write(png)
		default:
			return false
		}
		return true
	})
	var results []BatchItemResult
	for item, err := range server.client(t).BatchResults(context.Background(), testBatchID, time.Minute) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, item)
	}
	if len(results) != 2 || polls != 2 {
		t.Fatalf("results after %d polls: %+v", polls, results)
	}
	ok, rejected := results[0], results[1]
	if !ok.OK() || string(ok.Result.Bytes) != string(png) || ok.RequestID != testRequestID || *ok.Credits != 1 {
		t.Fatalf("succeeded item: %+v", ok)
	}
	if rejected.OK() || rejected.ErrorCode != "upload_size_mismatch" || rejected.Status != "rejected" || rejected.Credits != nil ||
		!strings.Contains(rejected.ErrorDetail, "size") {
		t.Fatalf("rejected item: %+v", rejected)
	}
}

func TestDownloadBatchArchiveWaitsForTheZip(t *testing.T) {
	zip := []byte("PK\x03\x04archive")
	attempts := 0
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.URL.Path != "/watermarks/batches/"+testBatchID+"/archive" {
			return false
		}
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0.05")
			writeJSON(w, 202, batchView("assembling"))
			return true
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zip)
		return true
	})
	got, err := server.client(t).DownloadBatchArchive(context.Background(), testBatchID, time.Minute)
	if err != nil || string(got) != string(zip) || attempts != 2 {
		t.Fatalf("archive: %q %v after %d", got, err, attempts)
	}

	missing := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		writeJSON(w, 409, map[string]any{"detail": map[string]any{"code": "archive_not_requested", "message": "This batch was created without archive: true"}})
		return true
	})
	_, err = missing.client(t).DownloadBatchArchive(context.Background(), testBatchID, time.Minute)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.Code != "archive_not_requested" {
		t.Fatalf("409: %v", err)
	}
}

func TestSubmitBatchZipCancelAndList(t *testing.T) {
	var manifest map[string]any
	var key, query string
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch r.Method + " " + r.URL.Path {
		case "POST /watermarks/batches/zip":
			key = r.Header.Get("Idempotency-Key")
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			files := r.MultipartForm.File["archive"]
			if len(files) != 1 || files[0].Header.Get("Content-Type") != "application/zip" {
				t.Errorf("archive part: %v", files)
			}
			_ = json.Unmarshal([]byte(r.FormValue("manifest")), &manifest)
			writeJSON(w, 202, batchView("starting"))
		case "POST /watermarks/batches/" + testBatchID + "/cancel":
			view := batchView("cancelled")
			view["cancel_requested"] = true
			writeJSON(w, 200, view)
		case "GET /watermarks/batches":
			query = r.URL.RawQuery
			writeJSON(w, 200, map[string]any{"data": []any{batchView("completed")}, "next_cursor": testBatchID})
		default:
			return false
		}
		return true
	})
	c := server.client(t)
	batch, err := c.SubmitBatchZip(context.Background(), []byte("PK\x03\x04"), []BatchZipItem{{Filename: "in/a.png", Data: map[string]any{"recipient": "a"}}},
		&BatchOptions{Accelerator: AcceleratorGPU})
	if err != nil || batch.Status != "starting" {
		t.Fatalf("zip: %+v %v", batch, err)
	}
	items := manifest["items"].([]any)
	if !idempotencyKey.MatchString(key) || manifest["accelerator"] != "gpu" || len(items) != 1 || items[0].(map[string]any)["filename"] != "in/a.png" {
		t.Fatalf("zip request: key %q manifest %v", key, manifest)
	}
	cancelled, err := c.CancelBatch(context.Background(), testBatchID)
	if err != nil || cancelled.Status != "cancelled" || !cancelled.CancelRequested {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	page, err := c.ListBatches(context.Background(), 10, testBatchID)
	if err != nil || len(page.Data) != 1 || *page.NextCursor != testBatchID || query != "before="+testBatchID+"&limit=10" {
		t.Fatalf("list: %+v %v %q", page, err, query)
	}
}

func timedClient(t *testing.T, s *batchServer, timeout time.Duration) *Client {
	t.Helper()
	c, err := New("test-key", WithBaseURL(s.URL), WithHTTPClient(s.Client()), WithTimeout(timeout))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestArchivePollingOutlastsTheClientTimeout(t *testing.T) {
	zip := []byte("PK\x05\x06" + strings.Repeat("z", 100))
	attempts := 0
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if attempts++; attempts == 1 {
			w.Header().Set("Retry-After", "0.2") // floored to 1 s
			writeJSON(w, 202, batchView("processing"))
			return true
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zip)
		return true
	})
	var out bytes.Buffer
	start := time.Now()
	n, err := timedClient(t, server, 300*time.Millisecond).DownloadBatchArchiveTo(context.Background(), testBatchID, &out, time.Minute)
	if err != nil || n != int64(len(zip)) || out.String() != string(zip) || attempts != 2 {
		t.Fatalf("archive: %d %v after %d attempts", n, err, attempts)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("Retry-After not honored: %s", elapsed)
	}
	if MaxBatchArchiveSize != 1<<30+64<<20 {
		t.Fatalf("archive cap %d", MaxBatchArchiveSize)
	}

	never := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		w.Header().Set("Retry-After", "0.05")
		writeJSON(w, 202, batchView("assembling"))
		return true
	})
	out.Reset()
	_, err = never.client(t).DownloadBatchArchiveTo(context.Background(), testBatchID, &out, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "not ready") || out.Len() != 0 {
		t.Fatalf("wait timeout: %v", err)
	}
}

func TestArchiveDownloadStallIsReported(t *testing.T) {
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("PK\x03\x04"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never sends the rest
		return true
	})
	_, err := timedClient(t, server, 200*time.Millisecond).DownloadBatchArchive(context.Background(), testBatchID, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("stall: %v", err)
	}
}

// slowReader yields one chunk per interval: progress that keeps flowing.
type slowReader struct {
	chunks   int
	interval time.Duration
}

func (r *slowReader) Read(b []byte) (int, error) {
	if r.chunks == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.interval)
	r.chunks--
	n := copy(b, strings.Repeat("s", 100))
	return n, nil
}

func TestSignedUploadUsesAnIdleTimeout(t *testing.T) {
	server := newBatchServer(t, func(http.ResponseWriter, *http.Request, []byte) bool { return false })
	c := timedClient(t, server, 300*time.Millisecond)
	start := time.Now()
	err := c.putSigned(context.Background(), server.URL+"/signed/slow?Signature=s", 1000, func() (io.ReadCloser, error) {
		return io.NopCloser(&slowReader{chunks: 10, interval: 100 * time.Millisecond}), nil
	})
	if err != nil || time.Since(start) < 900*time.Millisecond || len(server.puts["slow"]) != 1000 {
		t.Fatalf("slow upload: %v after %s", err, time.Since(start))
	}

	err = c.putSigned(context.Background(), server.URL+"/signed/stuck?Signature=s", 1000, func() (io.ReadCloser, error) {
		return io.NopCloser(&slowReader{chunks: 1, interval: 1500 * time.Millisecond}), nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("stalled upload: %v", err)
	}
}

func TestAFileThatChangedSizeFailsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(path, []byte("\x89PNG first"), 0o600); err != nil {
		t.Fatal(err)
	}
	var server *batchServer
	server = newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.URL.Path != "/watermarks/batches" {
			return false
		}
		_ = os.WriteFile(path, []byte("\x89PNG second, longer"), 0o600)
		writeJSON(w, 201, batchView("draft", map[string]any{"index": 0, "filename": "a.png", "status": "pending",
			"upload": map[string]any{"method": "PUT", "url": server.URL + "/signed/a.png?Signature=s", "expires_at": "x"}}))
		return true
	})
	start := time.Now()
	_, err := server.client(t).SubmitBatch(context.Background(), []BatchItem{{Path: path, Data: map[string]any{"a": 1}}}, nil)
	if !errors.Is(err, errSizeChanged) || time.Since(start) > 500*time.Millisecond || strings.Contains(server.callList(), "PUT ") {
		t.Fatalf("changed file: %v after %s, calls %s", err, time.Since(start), server.callList())
	}

	short := newBatchServer(t, func(http.ResponseWriter, *http.Request, []byte) bool { return false })
	start = time.Now()
	err = short.client(t).putSigned(context.Background(), short.URL+"/signed/short?Signature=s", 100, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("only ten b")), nil
	})
	if !errors.Is(err, errSizeChanged) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("short body: %v after %s", err, time.Since(start))
	}
}

func TestResumingAnExpiredOrStartedBatch(t *testing.T) {
	status := "expired"
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.URL.Path != "/watermarks/batches" {
			return false
		}
		writeJSON(w, 200, batchView(status, map[string]any{"index": 0, "filename": "a.png", "status": "pending"}))
		return true
	})
	c := server.client(t)
	items := []BatchItem{{Filename: "a.png", File: []byte("x"), Data: map[string]any{"a": 1}}}
	_, err := c.SubmitBatch(context.Background(), items, &BatchOptions{IdempotencyKey: "expired-key-1"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 410 || apiErr.Code != "batch_expired" || !strings.Contains(apiErr.Detail, "expired") ||
		apiErr.IdempotencyKey != "expired-key-1" {
		t.Fatalf("expired: %v", err)
	}
	status = "processing"
	batch, err := c.SubmitBatch(context.Background(), items, &BatchOptions{IdempotencyKey: "expired-key-1"})
	if err != nil || batch.Status != "processing" {
		t.Fatalf("started replay: %+v %v", batch, err)
	}
	if calls := server.callList(); calls != "POST /watermarks/batches,POST /watermarks/batches" {
		t.Fatalf("calls: %s", calls)
	}
}

func TestBatchResultsNameCancelledFiles(t *testing.T) {
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		writeJSON(w, 200, batchView("cancelled",
			map[string]any{"index": 0, "filename": "a.png", "status": "pending"},
			map[string]any{"index": 1, "filename": "b.png", "status": "failed", "error_code": "invalid_input", "credits": 0}))
		return true
	})
	var codes []string
	for item, err := range server.client(t).BatchResults(context.Background(), testBatchID, 0) {
		if err != nil || item.OK() {
			t.Fatalf("item %+v %v", item, err)
		}
		codes = append(codes, item.ErrorCode)
	}
	if strings.Join(codes, ",") != "cancelled,invalid_input" {
		t.Fatalf("codes: %v", codes)
	}
}

func TestADraftItemWithoutAnUploadLinkIsAnError(t *testing.T) {
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.URL.Path != "/watermarks/batches" {
			return false
		}
		// Neither an upload URL nor upload_received: its URL expired.
		writeJSON(w, 200, batchView("draft", map[string]any{"index": 0, "filename": "a.png", "status": "pending"}))
		return true
	})
	_, err := server.client(t).SubmitBatch(context.Background(), []BatchItem{{Filename: "a.png", File: []byte("x"), Data: map[string]any{"a": 1}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "a.png") || server.callList() != "POST /watermarks/batches" {
		t.Fatalf("missing link: %v, calls %s", err, server.callList())
	}
}

// slowWriter takes longer than the client timeout for every write.
type slowWriter struct {
	bytes.Buffer
	delay time.Duration
}

func (w *slowWriter) Write(b []byte) (int, error) {
	time.Sleep(w.delay)
	return w.Buffer.Write(b)
}

func TestArchiveWriterTimeIsNotIdleTime(t *testing.T) {
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK\x03\x04"))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("rest of the zip"))
		return true
	})
	sink := &slowWriter{delay: 400 * time.Millisecond}
	n, err := timedClient(t, server, 200*time.Millisecond).DownloadBatchArchiveTo(context.Background(), testBatchID, sink, time.Minute)
	if err != nil || n != int64(len("PK\x03\x04rest of the zip")) || sink.String() != "PK\x03\x04rest of the zip" {
		t.Fatalf("slow writer: %d %v %q", n, err, sink.String())
	}
}

func TestArchiveOverTheCapIsRefusedBeforeReading(t *testing.T) {
	server := newBatchServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.FormatInt(MaxBatchArchiveSize+1, 10))
		w.WriteHeader(200)
		_, _ = w.Write([]byte("PK"))
		return true
	})
	var out bytes.Buffer
	_, err := server.client(t).DownloadBatchArchiveTo(context.Background(), testBatchID, &out, time.Minute)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "archive_too_large" || !strings.Contains(err.Error(), "exceeds") || out.Len() != 0 ||
		server.callList() != "GET /watermarks/batches/"+testBatchID+"/archive" {
		t.Fatalf("cap: %v, calls %s", err, server.callList())
	}
}
