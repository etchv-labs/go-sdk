// Package etchv is the official Go client for the Etchv API: invisible,
// verifiable watermarking and detection for images, PDFs and videos, plus the
// asset library, webhook endpoints and customer storage destinations.
//
// The client authenticates with an API key sent in the X-API-Key header. Keep
// keys on your server; never ship them to browsers or mobile apps. A [Client]
// is safe for concurrent use by multiple goroutines.
//
// Every method that performs a request takes a [context.Context]. Each call is
// additionally bounded by the client timeout (two minutes by default). A
// client timeout or cancellation does not cancel work already accepted by the
// server; resume it with the returned request ID or by resending the request
// with the same idempotency key.
//
// Failed requests return an [*Error], which carries the HTTP status code, the
// API's detail message and the X-Request-ID of the failing request:
//
//	client, err := etchv.New(os.Getenv("ETCHV_API_KEY"))
//	if err != nil {
//		log.Fatal(err)
//	}
//	photo, err := os.ReadFile("photo.jpg")
//	if err != nil {
//		log.Fatal(err)
//	}
//	result, err := client.EmbedImage(ctx, photo,
//		map[string]any{"recipient": "customer-123"},
//		etchv.Options{Filename: "photo.jpg"})
//	var apiErr *etchv.Error
//	if errors.As(err, &apiErr) {
//		log.Fatalf("HTTP %d (request %s): %s", apiErr.StatusCode, apiErr.RequestID, apiErr.Detail)
//	} else if err != nil {
//		log.Fatal(err)
//	}
//	err = os.WriteFile(result.Filename, result.Bytes, 0o600)
package etchv

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Version is the SDK release version. It is sent in the User-Agent header.
const Version = "1.0.0"

// MaxFileSize is the largest upload the API accepts (50 MB). Images may use
// all of it; the API rejects PDFs and videos over 20 MB with status 413.
const MaxFileSize = 50 * 1024 * 1024

// MaxDownloadSize is the largest response or result file the client reads (256 MB).
const MaxDownloadSize = 256 * 1024 * 1024

// DefaultBaseURL is the production API endpoint used by [New].
const DefaultBaseURL = "https://api.etchv.com"

// DefaultTimeout is the per-call client deadline used by [New].
const DefaultTimeout = 2 * time.Minute

var (
	jobID          = regexp.MustCompile(`^req_[a-f0-9]{64}$`)
	watermarkID    = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	webhookID      = regexp.MustCompile(`^wh_[a-f0-9]{32}$`)
	eventID        = regexp.MustCompile(`^evt_[a-f0-9]{64}$`)
	destinationID  = regexp.MustCompile(`^dst_[a-f0-9]{32}$`)
	deliveryID     = regexp.MustCompile(`^std_[a-f0-9]{64}$`)
	idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	safeFilename   = regexp.MustCompile(`filename="([A-Za-z0-9._-]+)"`)
)

// Media selects the kind of file an operation accepts.
type Media string

// Supported media kinds.
const (
	MediaImages    Media = "images"
	MediaDocuments Media = "documents"
	MediaVideos    Media = "videos"
)

func (m Media) valid() bool { return m == MediaImages || m == MediaDocuments || m == MediaVideos }

// Accelerator selects the hardware that processes a watermarking or detection
// request. The zero value leaves the choice to the API (CPU).
type Accelerator string

// Supported accelerators.
const (
	// AcceleratorCPU processes the request on CPU (the default).
	AcceleratorCPU Accelerator = "cpu"
	// AcceleratorGPU requests GPU processing: Business plan or higher (HTTP 403
	// otherwise), charged at 3× credits. When no GPU is ready the request runs
	// on CPU at normal credits; results report the accelerator actually used.
	AcceleratorGPU Accelerator = "gpu"
)

func (a Accelerator) valid() bool { return a == AcceleratorCPU || a == AcceleratorGPU }

// accelerator returns a recognized accelerator value, or "" for anything else.
func accelerator(value string) Accelerator {
	if a := Accelerator(strings.ToLower(strings.TrimSpace(value))); a.valid() {
		return a
	}
	return ""
}

// Error is returned for every failed API request and for client-side
// transport failures or deadlines. Use [errors.As] to inspect it.
//
// StatusCode is 0 when no HTTP response was received (network failure,
// cancellation or client deadline); Unwrap then returns the underlying cause,
// so errors.Is(err, context.DeadlineExceeded) works. Error never contains the
// API key.
type Error struct {
	// StatusCode is the HTTP status, or 0 for client/transport failures.
	StatusCode int
	// Detail is the API's human-readable explanation, or a client-side description.
	Detail string
	// JobStatus is the job state reported with the error, such as "failed",
	// "expired" or "deleted", when the API includes one.
	JobStatus string
	// ErrorCode is the machine-readable failure code of a failed job, if any.
	ErrorCode string
	// RequestID is the X-Request-ID of the failing request or job, if known.
	RequestID string
	// IdempotencyKey is the key sent with a durable request, for recovery.
	IdempotencyKey string
	// Code is the machine-readable code from a structured error detail, such
	// as "rate_limited" or "concurrency_limited" for HTTP 429.
	Code string
	// Message is the human-readable message from a structured error detail.
	// When set, Error() reports it instead of Detail.
	Message string
	// Limit is the limit from a structured error detail, such as the request
	// or concurrency limit for HTTP 429, or zero when absent.
	Limit int
	// RetryAfter is the wait the API requested with an HTTP 429 response
	// (Retry-After), or zero when it sent none.
	RetryAfter time.Duration

	err error
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("etchv: ")
	text := e.Detail
	if e.Message != "" {
		text = e.Message
	}
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, "HTTP %d", e.StatusCode)
		if text != "" {
			b.WriteString(": ")
		}
	}
	b.WriteString(text)
	if e.err != nil {
		b.WriteString(": ")
		b.WriteString(e.err.Error())
	}
	if e.RequestID != "" {
		b.WriteString(" (request ")
		b.WriteString(e.RequestID)
		b.WriteString(")")
	}
	return b.String()
}

// Unwrap returns the underlying transport or context error, if any.
func (e *Error) Unwrap() error { return e.err }

// Options are per-request options for watermarking and detection.
type Options struct {
	// Filename is the original filename sent with the upload. It defaults to a
	// generic name for the media kind; send the real name when you have it.
	Filename string
	// IdempotencyKey makes a request safely repeatable: 8–128 letters, digits,
	// hyphens or underscores. Durable operations generate one when empty;
	// persist your own to recover across process restarts.
	IdempotencyKey string
	// WebhookID is an enabled webhook endpoint (wh_…) that receives the job's
	// terminal event. Only valid with SubmitEmbed and SubmitDetection.
	WebhookID string
	// StorageDestinationID selects a verified customer storage destination
	// (dst_…) for the watermarked result. Embedding only.
	StorageDestinationID string
	// StorageKey is an optional object key beneath the destination prefix.
	// Requires StorageDestinationID.
	StorageKey string
	// Accelerator requests CPU or GPU processing; empty uses the API default
	// (CPU). AcceleratorGPU requires a Business plan or higher and costs 3×
	// credits; without a ready GPU the request runs on CPU at normal credits.
	Accelerator Accelerator
}

// EmbedResult is a verified watermarked file in its original format.
type EmbedResult struct {
	// Bytes is the watermarked file; save it as-is.
	Bytes []byte
	// WatermarkID is the 64-character hexadecimal identifier embedded in the file.
	WatermarkID string
	// RequestID identifies the job (req_…).
	RequestID string
	// ContentType is the MIME type of Bytes.
	ContentType string
	// Filename is a safe suggested filename from the API.
	Filename string
	// AssetID is the asset library record of the watermarked output, if created.
	AssetID string
	// SourceAssetID is the asset library record of the original upload, if created.
	SourceAssetID string
	// StorageDeliveryID identifies the customer storage delivery, if one was selected.
	StorageDeliveryID string
	// Accelerator is the hardware that actually processed the file ("cpu" or
	// "gpu"), or empty when the API did not report it.
	Accelerator Accelerator
}

// DetectionUnit is the detection result for one frame, page or composite.
type DetectionUnit struct {
	Index       int     `json:"index"`
	Watermarked bool    `json:"watermarked"`
	Confidence  float64 `json:"confidence"`
	WatermarkID *string `json:"watermark_id"`
}

// DetectionResult reports whether a file carries an Etchv watermark. A
// top-level WatermarkID is set only when every unit agrees.
type DetectionResult struct {
	Watermarked bool            `json:"watermarked"`
	Confidence  float64         `json:"confidence"`
	WatermarkID *string         `json:"watermark_id"`
	Units       []DetectionUnit `json:"units"`
	RequestID   string          `json:"-"`
	// Accelerator is the hardware that actually processed the file ("cpu" or
	// "gpu"), or empty when the API did not report it.
	Accelerator Accelerator `json:"-"`
}

// Job is the receipt and status of a durable watermarking or detection job.
type Job struct {
	RequestID string `json:"request_id"`
	// Status is one of "queued", "running", "retrying", "succeeded" or "failed".
	Status string `json:"status"`
	// Operation is "embed" or "detect".
	Operation            string  `json:"operation"`
	StatusURL            string  `json:"status_url"`
	ResultURL            string  `json:"result_url"`
	WebhookID            *string `json:"webhook_id"`
	AssetID              *string `json:"asset_id"`
	SourceAssetID        *string `json:"source_asset_id"`
	Format               string  `json:"format"`
	FrameCount           int     `json:"frame_count"`
	Credits              int     `json:"credits"`
	Attempts             int     `json:"attempts"`
	ErrorCode            *string `json:"error_code"`
	ResultExpiresAt      *string `json:"result_expires_at"`
	StorageProvider      string  `json:"storage_provider"`
	StorageDestinationID *string `json:"storage_destination_id"`
	StorageDeliveryID    *string `json:"storage_delivery_id"`
	// AcceleratorRequested is the accelerator requested at submission ("cpu"
	// or "gpu").
	AcceleratorRequested Accelerator `json:"accelerator_requested"`
	// Accelerator is the hardware that processed (or is processing) the job;
	// it differs from AcceleratorRequested after an automatic CPU fallback.
	Accelerator Accelerator `json:"accelerator"`
}

// Done reports whether the job reached a terminal state.
func (j *Job) Done() bool { return j.Status == "succeeded" || j.Status == "failed" }

// APIKeyInfo describes the API key used by the client.
type APIKeyInfo struct {
	OrganizationID string   `json:"organization_id"`
	KeyID          string   `json:"key_id"`
	Scopes         []string `json:"scopes"`
}

// Client calls the Etchv API. Create one with [New] and reuse it.
type Client struct {
	key, baseURL string
	timeout      time.Duration
	http         *http.Client
}

// ClientOption configures a [Client] created by [New].
type ClientOption func(*clientConfig)

type clientConfig struct {
	baseURL string
	timeout time.Duration
	http    *http.Client
}

// WithBaseURL overrides the API base URL. It must use HTTPS; plain HTTP is
// allowed only for localhost.
func WithBaseURL(baseURL string) ClientOption {
	return func(c *clientConfig) { c.baseURL = baseURL }
}

// WithTimeout sets the per-call client deadline, including automatic retries
// and job polling. It does not cancel server work.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *clientConfig) { c.timeout = timeout }
}

// WithHTTPClient supplies the underlying HTTP client, for example to configure
// a proxy or transport. The client is copied; redirects are never followed.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *clientConfig) { c.http = client }
}

// New returns a client for the production API with a two-minute per-call
// deadline, adjusted by any options.
func New(apiKey string, opts ...ClientOption) (*Client, error) {
	cfg := clientConfig{baseURL: DefaultBaseURL, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}
	u, err := url.Parse(cfg.baseURL)
	local := u != nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && local)) {
		return nil, errors.New("etchv: base URL must use HTTPS (HTTP allowed for localhost) without credentials, query or fragment")
	}
	if strings.TrimSpace(apiKey) == "" || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("etchv: API key is required")
	}
	if cfg.timeout <= 0 {
		return nil, errors.New("etchv: timeout must be positive")
	}
	hc := &http.Client{}
	if cfg.http != nil {
		copied := *cfg.http
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{apiKey, strings.TrimRight(cfg.baseURL, "/"), cfg.timeout, hc}, nil
}

// CheckAPIKey validates the client's API key without consuming credits and
// returns its organization, key ID and scopes. It requires an active key but
// no particular scope.
func (c *Client) CheckAPIKey(ctx context.Context) (*APIKeyInfo, error) {
	var info APIKeyInfo
	if err := c.json(ctx, "GET", "auth/api-key", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// EmbedImage watermarks an image and waits for the verified result. data is
// non-empty forensic JSON; only its SHA-256 digest is embedded.
func (c *Client) EmbedImage(ctx context.Context, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	return c.embed(ctx, MediaImages, file, data, opts)
}

// EmbedDocument watermarks a PDF and waits for the verified result.
func (c *Client) EmbedDocument(ctx context.Context, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	return c.embed(ctx, MediaDocuments, file, data, opts)
}

// EmbedVideo watermarks an MP4 or MOV video and waits for the verified result.
func (c *Client) EmbedVideo(ctx context.Context, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	return c.embed(ctx, MediaVideos, file, data, opts)
}

// DetectImage detects a watermark in an image. Image detection is synchronous
// and is not retried automatically.
func (c *Client) DetectImage(ctx context.Context, file []byte, opts Options) (*DetectionResult, error) {
	return c.detect(ctx, MediaImages, file, opts)
}

// DetectDocument detects watermarks in a PDF, page by page. It is synchronous
// and is not retried automatically.
func (c *Client) DetectDocument(ctx context.Context, file []byte, opts Options) (*DetectionResult, error) {
	return c.detect(ctx, MediaDocuments, file, opts)
}

// DetectVideo detects watermarks in a video, frame by frame. It uses a durable
// job and waits for the result.
func (c *Client) DetectVideo(ctx context.Context, file []byte, opts Options) (*DetectionResult, error) {
	return c.detect(ctx, MediaVideos, file, opts)
}

// SubmitEmbed durably submits a background watermarking job and returns its
// receipt without waiting for processing. Set opts.WebhookID to receive a
// signed terminal event.
func (c *Client) SubmitEmbed(ctx context.Context, media Media, file []byte, data map[string]any, opts Options) (*Job, error) {
	encoded, err := encodeData(data)
	if err != nil {
		return nil, err
	}
	return c.submit(ctx, media, file, encoded, opts)
}

// SubmitDetection durably submits a background detection job and returns its
// receipt without waiting for processing.
func (c *Client) SubmitDetection(ctx context.Context, media Media, file []byte, opts Options) (*Job, error) {
	return c.submit(ctx, media, file, nil, opts)
}

// GetEmbedJob returns the current status of a watermarking job.
func (c *Client) GetEmbedJob(ctx context.Context, requestID string) (*Job, error) {
	return c.getJob(ctx, "jobs", requestID)
}

// GetDetectionJob returns the current status of a detection job.
func (c *Client) GetDetectionJob(ctx context.Context, requestID string) (*Job, error) {
	return c.getJob(ctx, "detection-jobs", requestID)
}

// GetEmbedResult retrieves a watermarking job's result, polling while the job
// is still processing (HTTP 202) until it finishes or the deadline expires.
// An expired or deleted result returns an [*Error] with StatusCode 410.
func (c *Client) GetEmbedResult(ctx context.Context, requestID string) (*EmbedResult, error) {
	if !jobID.MatchString(requestID) {
		return nil, errors.New("etchv: invalid request ID")
	}
	res, err := c.do(ctx, call{method: "GET", path: "watermarks/jobs/" + requestID + "/result", retry: true, poll: "jobs"})
	if err != nil {
		return nil, err
	}
	return embedding(res.body, res.header)
}

// GetDetectionResult retrieves a detection job's result, polling while the
// job is still processing (HTTP 202) until it finishes or the deadline expires.
func (c *Client) GetDetectionResult(ctx context.Context, requestID string) (*DetectionResult, error) {
	if !jobID.MatchString(requestID) {
		return nil, errors.New("etchv: invalid request ID")
	}
	res, err := c.do(ctx, call{method: "GET", path: "watermarks/detection-jobs/" + requestID + "/result", retry: true, poll: "detection-jobs"})
	if err != nil {
		return nil, err
	}
	return detection(res.body, res.header)
}

func encodeData(data map[string]any) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("etchv: data must be a non-empty JSON object")
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("etchv: encode data: %w", err)
	}
	return encoded, nil
}

func (c *Client) embed(ctx context.Context, media Media, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	encoded, err := encodeData(data)
	if err != nil {
		return nil, err
	}
	res, err := c.upload(ctx, media, file, encoded, opts, false)
	if err != nil {
		return nil, err
	}
	return embedding(res.body, res.header)
}

func (c *Client) detect(ctx context.Context, media Media, file []byte, opts Options) (*DetectionResult, error) {
	res, err := c.upload(ctx, media, file, nil, opts, false)
	if err != nil {
		return nil, err
	}
	return detection(res.body, res.header)
}

func (c *Client) submit(ctx context.Context, media Media, file, data []byte, opts Options) (*Job, error) {
	res, err := c.upload(ctx, media, file, data, opts, true)
	if err != nil {
		return nil, err
	}
	var job Job
	if json.Unmarshal(res.body, &job) != nil || !jobID.MatchString(job.RequestID) {
		return nil, &Error{StatusCode: res.status, Detail: "invalid job receipt", RequestID: res.header.Get("X-Request-ID"), IdempotencyKey: res.key}
	}
	return &job, nil
}

func (c *Client) getJob(ctx context.Context, prefix, id string) (*Job, error) {
	if !jobID.MatchString(id) {
		return nil, errors.New("etchv: invalid request ID")
	}
	var job Job
	if err := c.json(ctx, "GET", "watermarks/"+prefix+"/"+id, nil, nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// upload sends a multipart watermarking or detection request. data == nil
// selects detection.
func (c *Client) upload(ctx context.Context, media Media, file, data []byte, opts Options, async bool) (*response, error) {
	if !media.valid() {
		return nil, errors.New("etchv: invalid media type")
	}
	if len(file) == 0 || len(file) > MaxFileSize {
		return nil, errors.New("etchv: file must contain 1 byte to 50 MB")
	}
	if opts.IdempotencyKey != "" && !idempotencyKey.MatchString(opts.IdempotencyKey) {
		return nil, errors.New("etchv: idempotency key must contain 8–128 letters, digits, hyphens or underscores")
	}
	q := url.Values{}
	if opts.WebhookID != "" {
		if !async {
			return nil, errors.New("etchv: WebhookID requires SubmitEmbed or SubmitDetection")
		}
		if !webhookID.MatchString(opts.WebhookID) {
			return nil, errors.New("etchv: invalid webhook ID")
		}
		q.Set("webhook_id", opts.WebhookID)
	}
	if opts.StorageKey != "" && opts.StorageDestinationID == "" {
		return nil, errors.New("etchv: StorageKey requires StorageDestinationID")
	}
	if opts.StorageDestinationID != "" {
		if data == nil {
			return nil, errors.New("etchv: storage destinations apply to embedding only")
		}
		if !destinationID.MatchString(opts.StorageDestinationID) {
			return nil, errors.New("etchv: invalid storage destination ID")
		}
		q.Set("storage_destination_id", opts.StorageDestinationID)
		if opts.StorageKey != "" {
			q.Set("storage_key", opts.StorageKey)
		}
	}
	if opts.Accelerator != "" {
		if !opts.Accelerator.valid() {
			return nil, errors.New(`etchv: accelerator must be "cpu" or "gpu"`)
		}
		q.Set("accelerator", string(opts.Accelerator))
	}
	if opts.Filename == "" {
		opts.Filename = map[Media]string{MediaImages: "image.png", MediaDocuments: "document.pdf", MediaVideos: "video.mp4"}[media]
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", opts.Filename)
	if err != nil {
		return nil, err
	}
	if _, err = part.Write(file); err != nil {
		return nil, err
	}
	path := "watermarks/" + string(media)
	if data != nil {
		if err = w.WriteField("data", string(data)); err != nil {
			return nil, err
		}
	} else {
		path += "/detect"
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	if async {
		path += "/async"
	}
	// Embedding, video detection and all async submissions are durable jobs:
	// they are safe to retry with a stable idempotency key.
	durable := async || data != nil || media == MediaVideos
	if durable && opts.IdempotencyKey == "" {
		var key [16]byte
		if _, err = rand.Read(key[:]); err != nil {
			return nil, err
		}
		opts.IdempotencyKey = hex.EncodeToString(key[:])
	}
	r := call{method: "POST", path: path, query: q, body: body.Bytes(), contentType: w.FormDataContentType(), idempotencyKey: opts.IdempotencyKey, retry: durable}
	if durable && !async {
		r.poll = "jobs"
		if data == nil {
			r.poll = "detection-jobs"
		}
	}
	return c.do(ctx, r)
}

type call struct {
	method, path   string
	query          url.Values
	body           []byte
	contentType    string
	idempotencyKey string
	// retry retries transport failures and HTTP 429/502/503/504.
	retry bool
	// poll follows HTTP 202 job continuations to this job collection's result.
	poll string
}

type response struct {
	status int
	body   []byte
	header http.Header
	key    string
}

func retryDelay(h http.Header, fallback float64) time.Duration {
	delay := fallback
	if n, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
		delay = math.Max(.01, math.Min(5, n))
	}
	return time.Duration(delay * float64(time.Second))
}

// retryAfter parses a Retry-After value (delta-seconds or HTTP date) into a
// non-negative duration; it returns zero when the value is absent or invalid.
func retryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(value, 64); err == nil {
		if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 1e9 {
			return 0
		}
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(value); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func (c *Client) do(parent context.Context, r call) (*response, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	requestID := ""
	fail := func(status int, detail string, cause error) *Error {
		return &Error{StatusCode: status, Detail: detail, RequestID: requestID, IdempotencyKey: r.idempotencyKey, err: cause}
	}
	pause := func(delay time.Duration) {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	for ctx.Err() == nil {
		target := c.baseURL + "/" + r.path
		if len(r.query) > 0 {
			target += "?" + r.query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, r.method, target, bytes.NewReader(r.body))
		if err != nil {
			return nil, fail(0, "invalid request", err)
		}
		req.Header.Set("X-API-Key", c.key)
		req.Header.Set("User-Agent", "etchv-go/"+Version)
		if r.contentType != "" {
			req.Header.Set("Content-Type", r.contentType)
		}
		if r.idempotencyKey != "" && r.method == "POST" {
			req.Header.Set("Idempotency-Key", r.idempotencyKey)
		}
		res, err := c.http.Do(req)
		if err != nil {
			if !r.retry || ctx.Err() != nil {
				return nil, fail(0, "request failed", err)
			}
			pause(time.Second)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, MaxDownloadSize+1))
		res.Body.Close()
		if id := res.Header.Get("X-Request-ID"); id != "" {
			requestID = id
		}
		if err != nil {
			if !r.retry || ctx.Err() != nil {
				return nil, fail(0, "reading response failed", err)
			}
			pause(time.Second)
			continue
		}
		if len(b) > MaxDownloadSize {
			return nil, fail(res.StatusCode, "response exceeds 256 MB", nil)
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 && !(res.StatusCode == 202 && r.poll != "") {
			return &response{res.StatusCode, b, res.Header, r.idempotencyKey}, nil
		}
		var detail struct {
			Detail    json.RawMessage `json:"detail"`
			RequestID string          `json:"request_id"`
			Status    string          `json:"status"`
			ErrorCode string          `json:"error_code"`
		}
		_ = json.Unmarshal(b, &detail)
		if res.StatusCode == 202 {
			// Build the poll URL only from a validated ID, never from Location.
			if !jobID.MatchString(detail.RequestID) {
				return nil, fail(202, "invalid job response", nil)
			}
			requestID = detail.RequestID
			r = call{method: "GET", path: "watermarks/" + r.poll + "/" + requestID + "/result", idempotencyKey: r.idempotencyKey, retry: r.retry, poll: r.poll}
			pause(retryDelay(res.Header, 1))
			continue
		}
		if r.retry && (res.StatusCode == 429 || res.StatusCode == 502 || res.StatusCode == 503 || res.StatusCode == 504) && detail.Status != "failed" {
			pause(retryDelay(res.Header, 1))
			continue
		}
		e := fail(res.StatusCode, errorDetail(detail.Detail, b), nil)
		e.JobStatus, e.ErrorCode = detail.Status, detail.ErrorCode
		var structured struct {
			Code    string  `json:"code"`
			Message string  `json:"message"`
			Limit   float64 `json:"limit"`
		}
		if json.Unmarshal(detail.Detail, &structured) == nil {
			e.Code, e.Message, e.Limit = structured.Code, strings.ToValidUTF8(structured.Message, ""), int(structured.Limit)
		}
		if res.StatusCode == 429 {
			e.RetryAfter = retryAfter(res.Header.Get("Retry-After"), time.Now())
		}
		if e.RequestID == "" && jobID.MatchString(detail.RequestID) {
			e.RequestID = detail.RequestID
		}
		return nil, e
	}
	return nil, fail(0, "client deadline exceeded or context canceled; the job may still complete", ctx.Err())
}

// errorDetail prefers the API's string detail and falls back to a bounded body.
func errorDetail(raw json.RawMessage, body []byte) string {
	var s string
	if len(raw) > 0 && json.Unmarshal(raw, &s) == nil {
		body = []byte(s)
	} else if len(raw) > 0 && string(raw) != "null" {
		body = raw
	}
	const limit = 2000
	if len(body) > limit {
		body = body[:limit]
	}
	return strings.ToValidUTF8(string(body), "")
}

// json sends an optional JSON body and decodes an optional JSON response.
func (c *Client) json(ctx context.Context, method, path string, query url.Values, body, result any) error {
	r := call{method: method, path: path, query: query}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("etchv: encode request: %w", err)
		}
		r.body, r.contentType = b, "application/json"
	}
	res, err := c.do(ctx, r)
	if err != nil {
		return err
	}
	if result != nil && json.Unmarshal(res.body, result) != nil {
		return &Error{StatusCode: res.status, Detail: "invalid JSON response", RequestID: res.header.Get("X-Request-ID")}
	}
	return nil
}

// Download is a file downloaded from the asset library or customer storage.
type Download struct {
	// Bytes is the file content.
	Bytes []byte
	// ContentType is the MIME type reported by the API.
	ContentType string
	// Filename is the attachment filename without any directory components,
	// or empty when the API sent none.
	Filename string
	// AssetID is the asset the file belongs to, if reported.
	AssetID string
}

func (c *Client) download(ctx context.Context, path string) (*Download, error) {
	res, err := c.do(ctx, call{method: "GET", path: path})
	if err != nil {
		return nil, err
	}
	d := &Download{Bytes: res.body, ContentType: strings.TrimSpace(strings.Split(res.header.Get("Content-Type"), ";")[0]), AssetID: res.header.Get("X-Asset-ID")}
	if _, params, err := mime.ParseMediaType(res.header.Get("Content-Disposition")); err == nil {
		name := params["filename"]
		if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00") && !strings.ContainsFunc(name, func(r rune) bool { return r < 32 || r == 127 }) {
			d.Filename = name
		}
	}
	return d, nil
}

func embedding(b []byte, h http.Header) (*EmbedResult, error) {
	contentType := strings.Split(h.Get("Content-Type"), ";")[0]
	ext := extension(b, contentType)
	if ext == "" || !watermarkID.MatchString(h.Get("X-Watermark-ID")) {
		return nil, &Error{StatusCode: 200, Detail: "invalid embedding response", RequestID: h.Get("X-Request-ID")}
	}
	filename := "watermarked." + ext
	if m := safeFilename.FindStringSubmatch(h.Get("Content-Disposition")); m != nil {
		filename = m[1]
	}
	return &EmbedResult{b, h.Get("X-Watermark-ID"), h.Get("X-Request-ID"), contentType, filename, h.Get("X-Asset-ID"), h.Get("X-Source-Asset-ID"), h.Get("X-Storage-Delivery-ID"), accelerator(h.Get("X-Etchv-Accelerator"))}, nil
}

func validDetection(raw json.RawMessage) bool {
	var d struct {
		Watermarked *bool           `json:"watermarked"`
		Confidence  *float64        `json:"confidence"`
		WatermarkID json.RawMessage `json:"watermark_id"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Watermarked == nil || d.Confidence == nil || *d.Confidence < 0 || *d.Confidence > 1 || len(d.WatermarkID) == 0 {
		return false
	}
	var id string
	if *d.Watermarked {
		return json.Unmarshal(d.WatermarkID, &id) == nil && watermarkID.MatchString(id)
	}
	return string(d.WatermarkID) == "null"
}

func detection(b []byte, h http.Header) (*DetectionResult, error) {
	bad := &Error{StatusCode: 200, Detail: "invalid detection response", RequestID: h.Get("X-Request-ID")}
	if !validDetection(b) {
		return nil, bad
	}
	var d DetectionResult
	if json.Unmarshal(b, &d) != nil {
		return nil, bad
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if units, ok := raw["units"]; ok {
		var list []json.RawMessage
		if json.Unmarshal(units, &list) != nil || len(list) == 0 {
			return nil, bad
		}
		for i, u := range list {
			var idx struct {
				Index *int `json:"index"`
			}
			if !validDetection(u) || json.Unmarshal(u, &idx) != nil || idx.Index == nil || *idx.Index != i {
				return nil, bad
			}
		}
	} else {
		d.Units = []DetectionUnit{{0, d.Watermarked, d.Confidence, d.WatermarkID}}
	}
	d.RequestID = h.Get("X-Request-ID")
	d.Accelerator = accelerator(h.Get("X-Etchv-Accelerator"))
	if d.Accelerator == "" {
		var body string
		if json.Unmarshal(raw["accelerator"], &body) == nil {
			d.Accelerator = accelerator(body)
		}
	}
	return &d, nil
}

func extension(b []byte, contentType string) string {
	s := string(b)
	switch contentType {
	case "image/png":
		if strings.HasPrefix(s, "\x89PNG\r\n\x1a\n") {
			return "png"
		}
	case "image/jpeg":
		if strings.HasPrefix(s, "\xff\xd8\xff") {
			return "jpg"
		}
	case "image/gif":
		if strings.HasPrefix(s, "GIF87a") || strings.HasPrefix(s, "GIF89a") {
			return "gif"
		}
	case "image/tiff":
		if strings.HasPrefix(s, "II*\x00") || strings.HasPrefix(s, "MM\x00*") {
			return "tiff"
		}
	case "image/bmp":
		if strings.HasPrefix(s, "BM") {
			return "bmp"
		}
	case "image/x-portable-pixmap":
		if strings.HasPrefix(s, "P6") || strings.HasPrefix(s, "P3") {
			return "ppm"
		}
	case "image/webp":
		if len(b) >= 12 && s[:4] == "RIFF" && s[8:12] == "WEBP" {
			return "webp"
		}
	case "image/vnd.adobe.photoshop":
		if len(b) >= 6 && s[:5] == "8BPS\x00" {
			if b[5] == 1 {
				return "psd"
			}
			if b[5] == 2 {
				return "psb"
			}
		}
	case "application/pdf":
		if strings.HasPrefix(s, "%PDF-") {
			return "pdf"
		}
	case "video/mp4", "video/quicktime":
		if len(b) >= 8 && s[4:8] == "ftyp" {
			if contentType == "video/mp4" {
				return "mp4"
			}
			return "mov"
		}
	}
	return ""
}

// WebhookEndpoint is a registered webhook endpoint.
type WebhookEndpoint struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	// SigningSecret (whsec_…) is returned only once, by CreateWebhook. Store it
	// securely; it verifies deliveries with VerifyWebhook.
	SigningSecret string `json:"signing_secret,omitempty"`
}

// WebhookAttempt is one recorded delivery attempt.
type WebhookAttempt struct {
	At         string  `json:"at"`
	StatusCode *int    `json:"status_code"`
	Error      *string `json:"error"`
}

// WebhookDelivery is a queued or completed event delivery to an endpoint.
type WebhookDelivery struct {
	// ID is the event ID (evt_…).
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	// Status is "queued", "delivering", "retrying", "delivered", "exhausted" or "cancelled".
	Status        string           `json:"status"`
	Attempts      int              `json:"attempts"`
	CreatedAt     string           `json:"created_at"`
	NextAttemptAt *string          `json:"next_attempt_at"`
	History       []WebhookAttempt `json:"history"`
	// Payload is the exact event body sent to the endpoint.
	Payload json.RawMessage `json:"payload"`
}

// WebhookDeliveryPage is one page of deliveries, newest first. Pass
// NextCursor to ListWebhookDeliveries to continue; nil means the last page.
type WebhookDeliveryPage struct {
	Data       []WebhookDelivery `json:"data"`
	NextCursor *string           `json:"next_cursor"`
}

func webhookPath(id string) (string, error) {
	if !webhookID.MatchString(id) {
		return "", errors.New("etchv: invalid webhook ID")
	}
	return "webhooks/" + id, nil
}

// ListWebhooks returns the organization's webhook endpoints (webhooks:read).
func (c *Client) ListWebhooks(ctx context.Context) ([]WebhookEndpoint, error) {
	var list []WebhookEndpoint
	if err := c.json(ctx, "GET", "webhooks", nil, nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// CreateWebhook registers a public HTTPS endpoint (webhooks:write, owner or
// admin). The result's SigningSecret is shown only once.
func (c *Client) CreateWebhook(ctx context.Context, endpointURL string) (*WebhookEndpoint, error) {
	var e WebhookEndpoint
	if err := c.json(ctx, "POST", "webhooks", nil, map[string]string{"url": endpointURL}, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// UpdateWebhook enables or disables an endpoint (webhooks:write).
func (c *Client) UpdateWebhook(ctx context.Context, id string, enabled bool) (*WebhookEndpoint, error) {
	path, err := webhookPath(id)
	if err != nil {
		return nil, err
	}
	var e WebhookEndpoint
	if err := c.json(ctx, "PATCH", path, nil, map[string]bool{"enabled": enabled}, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// DeleteWebhook permanently deletes an endpoint (webhooks:write).
func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	path, err := webhookPath(id)
	if err != nil {
		return err
	}
	return c.json(ctx, "DELETE", path, nil, nil, nil)
}

// ListWebhookDeliveries returns up to 50 deliveries for an endpoint, newest
// first (webhooks:read). Pass an empty cursor for the first page.
func (c *Client) ListWebhookDeliveries(ctx context.Context, id, cursor string) (*WebhookDeliveryPage, error) {
	path, err := webhookPath(id)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if cursor != "" {
		if !eventID.MatchString(cursor) {
			return nil, errors.New("etchv: invalid delivery cursor")
		}
		q.Set("after", cursor)
	}
	var page WebhookDeliveryPage
	if err := c.json(ctx, "GET", path+"/deliveries", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// RedeliverWebhook queues a delivered, exhausted or cancelled event for
// another delivery (webhooks:write). The endpoint must be enabled.
func (c *Client) RedeliverWebhook(ctx context.Context, id, eventIdentifier string) error {
	path, err := webhookPath(id)
	if err != nil {
		return err
	}
	if !eventID.MatchString(eventIdentifier) {
		return errors.New("etchv: invalid event ID")
	}
	return c.json(ctx, "POST", path+"/deliveries/"+eventIdentifier+"/redeliver", nil, nil, nil)
}

// WebhookEvent is a verified webhook event.
type WebhookEvent struct {
	// ID is the stable event ID (evt_…); deduplicate on it.
	ID string `json:"id"`
	// Type is, for example, "watermark.embed.succeeded" or "storage.delivery.failed".
	Type       string `json:"type"`
	APIVersion string `json:"api_version"`
	CreatedAt  string `json:"created_at"`
	// Data is the event snapshot. Watermark events decode into [Job] (plus a
	// watermark_id field); storage events carry delivery fields.
	Data json.RawMessage `json:"data"`
}

// ErrInvalidWebhook is returned by VerifyWebhook for any delivery that fails
// verification.
var ErrInvalidWebhook = errors.New("etchv: invalid webhook signature or payload")

// WebhookTolerance is the maximum allowed difference between a delivery's
// X-Etchv-Timestamp and the local clock.
const WebhookTolerance = 5 * time.Minute

// VerifyWebhook authenticates a webhook delivery and returns its event. Pass
// the raw, unparsed request body, the request headers and the endpoint's
// signing secret (including the whsec_ prefix). It checks the
// X-Etchv-Signature HMAC-SHA256 in constant time, rejects timestamps outside
// [WebhookTolerance], and checks that the body ID matches X-Etchv-Event-ID.
func VerifyWebhook(body []byte, header http.Header, signingSecret string) (*WebhookEvent, error) {
	return verifyWebhook(body, header, signingSecret, time.Now())
}

func verifyWebhook(body []byte, header http.Header, signingSecret string, now time.Time) (*WebhookEvent, error) {
	timestamp := header.Get("X-Etchv-Timestamp")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if signingSecret == "" || err != nil || strings.HasPrefix(timestamp, "+") {
		return nil, ErrInvalidWebhook
	}
	if age := now.Sub(time.Unix(seconds, 0)); age > WebhookTolerance || age < -WebhookTolerance {
		return nil, ErrInvalidWebhook
	}
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	expected := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(header.Get("X-Etchv-Signature"))) {
		return nil, ErrInvalidWebhook
	}
	var event WebhookEvent
	if json.Unmarshal(body, &event) != nil || event.ID == "" || event.ID != header.Get("X-Etchv-Event-ID") {
		return nil, ErrInvalidWebhook
	}
	return &event, nil
}
