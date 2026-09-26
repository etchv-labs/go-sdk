package etchv

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestContract(t *testing.T) {
	base, formats := testServer(t)
	var err error
	ctx := context.Background()
	data := map[string]any{"asset": "example"}
	for _, f := range formats {
		c, _ := New("test-key", WithBaseURL(base+"/formats/"+f.Extension), WithTimeout(time.Second*10))
		b, _ := base64.StdEncoding.DecodeString(f.Base64)
		opts := Options{Filename: "input." + f.Extension}
		var result *EmbedResult
		var detected *DetectionResult
		switch f.Media {
		case "images":
			result, err = c.EmbedImage(ctx, b, data, opts)
		case "documents":
			result, err = c.EmbedDocument(ctx, b, data, opts)
		case "videos":
			result, err = c.EmbedVideo(ctx, b, data, opts)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(result.Bytes, b) || result.ContentType != f.Mime || result.Filename != "protected."+f.Extension {
			t.Fatal("native bytes or metadata mismatch")
		}
		switch f.Media {
		case "images":
			detected, err = c.DetectImage(ctx, b, opts)
		case "documents":
			detected, err = c.DetectDocument(ctx, b, opts)
		case "videos":
			detected, err = c.DetectVideo(ctx, b, opts)
		}
		if err != nil || len(detected.Units) != 2 || !detected.Watermarked {
			t.Fatalf("detection: %v", err)
		}
	}
	for _, scenario := range []string{"retry", "embed-job", "detect-job", "failed", "redirect", "invalid", "bad-job", "bad-id", "bad-detection", "bad-units", "deadline"} {
		ext := "png"
		if scenario == "embed-job" {
			ext = "pdf"
		}
		if scenario == "detect-job" {
			ext = "mp4"
		}
		var b []byte
		for _, f := range formats {
			if f.Extension == ext {
				b, _ = base64.StdEncoding.DecodeString(f.Base64)
			}
		}
		timeout := 10 * time.Second
		if scenario == "deadline" {
			timeout = 80 * time.Millisecond
		}
		c, _ := New("test-key", WithBaseURL(base+"/"+scenario+"/"+ext), WithTimeout(timeout))
		opts := Options{Filename: "input." + ext, IdempotencyKey: "stable-key"}
		switch scenario {
		case "embed-job":
			_, err = c.EmbedDocument(ctx, b, data, opts)
		case "detect-job":
			_, err = c.DetectVideo(ctx, b, opts)
		case "bad-detection", "bad-units":
			_, err = c.DetectImage(ctx, b, opts)
		default:
			_, err = c.EmbedImage(ctx, b, data, opts)
		}
		if scenario == "retry" || scenario == "embed-job" || scenario == "detect-job" {
			if err != nil {
				t.Fatal(scenario, err)
			}
		} else {
			var e *Error
			if !errors.As(err, &e) {
				t.Fatal("expected SDK error", scenario, err)
			}
			if scenario == "deadline" && (e.StatusCode != 0 || e.IdempotencyKey != "stable-key" || e.RequestID == "" || !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatal("missing recovery identifiers")
			}
		}
	}
}
func TestInputValidation(t *testing.T) {
	for _, base := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?x=1"} {
		if _, err := New("key", WithBaseURL(base), WithTimeout(time.Second)); err == nil {
			t.Fatal("unsafe URL accepted")
		}
	}
	c, _ := New("test-key")
	if _, err := c.GetEmbedResult(context.Background(), "../../steal"); err == nil {
		t.Fatal("invalid job accepted")
	}
	if _, err := c.EmbedImage(context.Background(), nil, map[string]any{"a": 1}, Options{}); err == nil {
		t.Fatal("empty file accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.EmbedImage(ctx, []byte("x"), map[string]any{"a": 1}, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled call accepted", err)
	}
	if _, err := New(" "); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := New("key", WithTimeout(0)); err == nil {
		t.Fatal("zero timeout accepted")
	}
	custom := &http.Client{Timeout: time.Second}
	c, err := New("key", WithHTTPClient(custom), WithBaseURL("http://localhost:1"))
	if err != nil || c.http == custom || c.http.CheckRedirect == nil || custom.CheckRedirect != nil {
		t.Fatal("custom HTTP client must be copied with redirects disabled")
	}
	_, err = c.CheckAPIKey(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 0 || errors.Unwrap(err) == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatal("transport errors must be wrapped", err)
	}
}
