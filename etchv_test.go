package etchv

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func TestContract(t *testing.T) {
	base, formats := testServer(t)
	var err error
	ctx := context.Background()
	data := map[string]any{"asset": "example"}
	for _, f := range formats {
		c, _ := NewWithOptions("test-key", base+"/formats/"+f.Extension, time.Second*10)
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
		c, _ := NewWithOptions("test-key", base+"/"+scenario+"/"+ext, timeout)
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
			if scenario == "deadline" && (e.StatusCode != 0 || e.IdempotencyKey != "stable-key" || e.RequestID == "") {
				t.Fatal("missing recovery identifiers")
			}
		}
	}
}
func TestInputValidation(t *testing.T) {
	for _, base := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?x=1"} {
		if _, err := NewWithOptions("key", base, time.Second); err == nil {
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
	if _, err := c.EmbedImage(ctx, []byte("x"), map[string]any{"a": 1}, Options{}); err == nil {
		t.Fatal("cancelled call accepted")
	}
}
