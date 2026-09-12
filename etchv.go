// Package etchv provides a server-side client for native media watermarking.
package etchv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxFileSize = 20 * 1024 * 1024

var jobID = regexp.MustCompile(`^req_[a-f0-9]{64}$`)
var watermarkID = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
var safeFilename = regexp.MustCompile(`filename="([A-Za-z0-9._-]+)"`)

type Error struct {
	StatusCode     int
	Detail         string
	RequestID      string
	IdempotencyKey string
}

func (e *Error) Error() string { return fmt.Sprintf("Etchv request failed (HTTP %d)", e.StatusCode) }

type Options struct {
	Filename       string
	IdempotencyKey string
}
type EmbedResult struct {
	Bytes                                                                 []byte
	WatermarkID, RequestID, ContentType, Filename, AssetID, SourceAssetID string
}
type DetectionUnit struct {
	Index       int     `json:"index"`
	Watermarked bool    `json:"watermarked"`
	Confidence  float64 `json:"confidence"`
	WatermarkID *string `json:"watermark_id"`
}
type DetectionResult struct {
	Watermarked bool            `json:"watermarked"`
	Confidence  float64         `json:"confidence"`
	WatermarkID *string         `json:"watermark_id"`
	Units       []DetectionUnit `json:"units"`
	RequestID   string          `json:"-"`
}
type Client struct {
	key, baseURL string
	timeout      time.Duration
	http         *http.Client
}

// New uses the production API and a two-minute client deadline. A timeout does not cancel a server job.
func New(apiKey string) (*Client, error) {
	return NewWithOptions(apiKey, "https://api.etchv.com", 2*time.Minute)
}
func NewWithOptions(apiKey, baseURL string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return nil, fmt.Errorf("base URL must use HTTPS (HTTP allowed for localhost)")
	}
	if strings.TrimSpace(apiKey) == "" || strings.ContainsAny(apiKey, "\r\n") || timeout <= 0 {
		return nil, fmt.Errorf("API key and positive timeout are required")
	}
	return &Client{apiKey, strings.TrimRight(baseURL, "/"), timeout, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) EmbedImage(ctx context.Context, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	return c.embed(ctx, "images", file, data, opts)
}
func (c *Client) EmbedDocument(ctx context.Context, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	return c.embed(ctx, "documents", file, data, opts)
}
func (c *Client) EmbedVideo(ctx context.Context, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	return c.embed(ctx, "videos", file, data, opts)
}
func (c *Client) DetectImage(ctx context.Context, file []byte, opts Options) (*DetectionResult, error) {
	return c.detect(ctx, "images", file, opts)
}
func (c *Client) DetectDocument(ctx context.Context, file []byte, opts Options) (*DetectionResult, error) {
	return c.detect(ctx, "documents", file, opts)
}
func (c *Client) DetectVideo(ctx context.Context, file []byte, opts Options) (*DetectionResult, error) {
	return c.detect(ctx, "videos", file, opts)
}
func (c *Client) GetEmbedResult(ctx context.Context, id string) (*EmbedResult, error) {
	if !jobID.MatchString(id) {
		return nil, fmt.Errorf("invalid request ID")
	}
	b, h, err := c.request(ctx, "watermarks/jobs/"+id+"/result", "GET", nil, "", "", true, false)
	if err != nil {
		return nil, err
	}
	return embedding(b, h)
}
func (c *Client) GetDetectionResult(ctx context.Context, id string) (*DetectionResult, error) {
	if !jobID.MatchString(id) {
		return nil, fmt.Errorf("invalid request ID")
	}
	b, h, err := c.request(ctx, "watermarks/detection-jobs/"+id+"/result", "GET", nil, "", "", true, true)
	if err != nil {
		return nil, err
	}
	return detection(b, h)
}
func (c *Client) embed(ctx context.Context, media string, file []byte, data map[string]any, opts Options) (*EmbedResult, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("data must be a non-empty JSON object")
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	b, h, err := c.post(ctx, media, file, encoded, opts)
	if err != nil {
		return nil, err
	}
	return embedding(b, h)
}
func (c *Client) detect(ctx context.Context, media string, file []byte, opts Options) (*DetectionResult, error) {
	b, h, err := c.post(ctx, media, file, nil, opts)
	if err != nil {
		return nil, err
	}
	return detection(b, h)
}
func (c *Client) post(ctx context.Context, media string, file, data []byte, opts Options, asyncWebhook ...string) ([]byte, http.Header, error) {
	if len(file) == 0 || len(file) > MaxFileSize {
		return nil, nil, fmt.Errorf("file must contain 1 byte to 20 MB")
	}
	if opts.Filename == "" {
		opts.Filename = map[string]string{"images": "image.png", "documents": "document.pdf", "videos": "video.mp4"}[media]
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", opts.Filename)
	if err != nil {
		return nil, nil, err
	}
	if _, err = part.Write(file); err != nil {
		return nil, nil, err
	}
	path := "watermarks/" + media
	if data != nil {
		if err = w.WriteField("data", string(data)); err != nil {
			return nil, nil, err
		}
	} else {
		path += "/detect"
	}
	if err = w.Close(); err != nil {
		return nil, nil, err
	}
	if len(asyncWebhook) > 0 {
		path += "/async"
		if asyncWebhook[0] != "" {
			path += "?webhook_id=" + asyncWebhook[0]
		}
	}
	durable := len(asyncWebhook) > 0 || data != nil || media == "videos"
	if durable && opts.IdempotencyKey == "" {
		var key [16]byte
		if _, err = rand.Read(key[:]); err != nil {
			return nil, nil, err
		}
		opts.IdempotencyKey = hex.EncodeToString(key[:])
	}
	return c.request(ctx, path, "POST", body.Bytes(), w.FormDataContentType(), opts.IdempotencyKey, durable, data == nil && media == "videos")
}
func (c *Client) request(parent context.Context, path, method string, body []byte, contentType, key string, durable, detectionJob bool) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	requestID := ""
	pause := func(delay time.Duration) {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+path, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("X-API-Key", c.key)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, err := c.http.Do(req)
		if err != nil {
			if !durable {
				return nil, nil, err
			}
			pause(time.Second)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, MaxFileSize+1))
		res.Body.Close()
		if id := res.Header.Get("X-Request-ID"); id != "" {
			requestID = id
		}
		if err != nil {
			if !durable {
				return nil, nil, err
			}
			pause(time.Second)
			continue
		}
		if len(b) > MaxFileSize {
			return nil, nil, &Error{res.StatusCode, "Response exceeds 20 MB", requestID, key}
		}
		if res.StatusCode == 200 || res.StatusCode == 204 || (res.StatusCode == 202 && strings.HasSuffix(strings.Split(path, "?")[0], "/async")) {
			return b, res.Header, nil
		}
		var detail struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
		}
		_ = json.Unmarshal(b, &detail)
		if durable && res.StatusCode == 202 {
			if !jobID.MatchString(detail.RequestID) {
				return nil, nil, &Error{202, "Invalid job response", requestID, key}
			}
			requestID = detail.RequestID
			prefix := "jobs"
			if detectionJob {
				prefix = "detection-jobs"
			}
			path = "watermarks/" + prefix + "/" + requestID + "/result"
			method = "GET"
			body = nil
			contentType = ""
			delay := 1.0
			if n, e := strconv.ParseFloat(res.Header.Get("Retry-After"), 64); e == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
				delay = math.Max(.01, math.Min(5, n))
			}
			pause(time.Duration(delay * float64(time.Second)))
			continue
		}
		if durable && (res.StatusCode == 429 || res.StatusCode == 502 || res.StatusCode == 503 || res.StatusCode == 504) && detail.Status != "failed" {
			pause(time.Second)
			continue
		}
		if len(b) > 10000 {
			b = b[:10000]
		}
		return nil, nil, &Error{res.StatusCode, string(b), requestID, key}
	}
	return nil, nil, &Error{0, "Client deadline exceeded or context cancelled; job may still complete", requestID, key}
}
func embedding(b []byte, h http.Header) (*EmbedResult, error) {
	mime := strings.Split(h.Get("Content-Type"), ";")[0]
	ext := extension(b, mime)
	if ext == "" || !watermarkID.MatchString(h.Get("X-Watermark-ID")) {
		return nil, &Error{200, "Invalid embedding response", h.Get("X-Request-ID"), ""}
	}
	filename := "watermarked." + ext
	if m := safeFilename.FindStringSubmatch(h.Get("Content-Disposition")); m != nil {
		filename = m[1]
	}
	return &EmbedResult{b, h.Get("X-Watermark-ID"), h.Get("X-Request-ID"), mime, filename, h.Get("X-Asset-ID"), h.Get("X-Source-Asset-ID")}, nil
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
	bad := &Error{200, "Invalid detection response", h.Get("X-Request-ID"), ""}
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
	return &d, nil
}
func extension(b []byte, mime string) string {
	s := string(b)
	switch mime {
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
			if mime == "video/mp4" {
				return "mp4"
			}
			return "mov"
		}
	}
	return ""
}

// SubmitEmbed accepts a durable job and returns its JSON receipt without polling.
func (c *Client) SubmitEmbed(ctx context.Context, media string, file []byte, data map[string]any, opts Options, webhookID string) (map[string]any, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("data must be a non-empty JSON object")
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return c.submit(ctx, media, file, encoded, opts, webhookID)
}
func (c *Client) SubmitDetection(ctx context.Context, media string, file []byte, opts Options, webhookID string) (map[string]any, error) {
	return c.submit(ctx, media, file, nil, opts, webhookID)
}
func (c *Client) submit(ctx context.Context, media string, file, data []byte, opts Options, webhookID string) (map[string]any, error) {
	if media != "images" && media != "documents" && media != "videos" {
		return nil, fmt.Errorf("invalid media type")
	}
	if webhookID != "" && !regexp.MustCompile(`^wh_[a-f0-9]{32}$`).MatchString(webhookID) {
		return nil, fmt.Errorf("invalid webhook ID")
	}
	b, _, err := c.post(ctx, media, file, data, opts, webhookID)
	if err != nil {
		return nil, err
	}
	var job map[string]any
	err = json.Unmarshal(b, &job)
	return job, err
}
func (c *Client) GetJob(ctx context.Context, id string, detect bool) (map[string]any, error) {
	if !jobID.MatchString(id) {
		return nil, fmt.Errorf("invalid request ID")
	}
	prefix := "jobs"
	if detect {
		prefix = "detection-jobs"
	}
	b, _, err := c.request(ctx, "watermarks/"+prefix+"/"+id, "GET", nil, "", "", false, detect)
	if err != nil {
		return nil, err
	}
	var job map[string]any
	err = json.Unmarshal(b, &job)
	return job, err
}
