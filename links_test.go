package etchv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testLinkAsset = "ast_" + strings.Repeat("e", 64)
	testLink      = "lnk_" + strings.Repeat("a", 32)
	testLinkEvent = "lev_" + strings.Repeat("b", 32)
	testLinkHook  = "wh_" + strings.Repeat("c", 32)
)

func linkServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing authentication")
		}
		var body map[string]any
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		w.Header().Set("Content-Type", "application/json")
		handle(w, r, body)
	}))
	t.Cleanup(server.Close)
	c, _ := New("test-key", WithBaseURL(server.URL), WithTimeout(5*time.Second))
	return c
}

func TestCreateLinkSendsTheBodyAndKeepsOneKeyAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var last map[string]any
	c := linkServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "POST" || r.URL.Path != "/links" {
			t.Error("unexpected request", r.Method, r.URL.Path)
		}
		keys, last = append(keys, r.Header.Get("Idempotency-Key")), body
		if len(keys) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"detail":"busy"}`))
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"` + testLink + `","url":"https://etchv.com/l/Yp3kQ8vN2rT6wX1zA4bC7d","status":"active","download_limit":3,"recipients":[]}`))
	})
	link, err := c.CreateLink(context.Background(), LinkCreate{AssetID: testLinkAsset, Name: "Board pack", Recipients: []string{"alex@example.com"},
		ExpiresAt: time.Date(2026, 11, 26, 14, 2, 0, 0, time.UTC), Prepare: "on_open", DownloadLimit: 3, WebhookID: testLinkHook})
	if err != nil {
		t.Fatal(err)
	}
	if link.ID != testLink || *link.DownloadLimit != 3 || len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("link %+v keys %v", link, keys)
	}
	want := map[string]any{"asset_id": testLinkAsset, "name": "Board pack", "recipients": []any{"alex@example.com"},
		"expires_at": "2026-11-26T14:02:00Z", "prepare": "on_open", "download_limit": 3.0, "webhook_id": testLinkHook}
	if !reflect.DeepEqual(last, want) {
		t.Fatalf("body %v", last)
	}
}

func TestCreateLinkForDomainsWithAGivenKey(t *testing.T) {
	c := linkServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Header.Get("Idempotency-Key") != "board-pack-2026-10" {
			t.Error("key", r.Header.Get("Idempotency-Key"))
		}
		if !reflect.DeepEqual(body, map[string]any{"asset_id": testLinkAsset, "name": "Board pack", "allowed_domains": []any{"example.com"}}) {
			t.Error("body", body)
		}
		_, _ = w.Write([]byte(`{"id":"` + testLink + `"}`))
	})
	if _, err := c.CreateLink(context.Background(), LinkCreate{AssetID: testLinkAsset, Name: "Board pack",
		AllowedDomains: []string{"example.com"}, IdempotencyKey: "board-pack-2026-10"}); err != nil {
		t.Fatal(err)
	}
}

func TestTooFewCreditsIsA402WithItsCodeAndNotRetried(t *testing.T) {
	var calls int
	c := linkServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		calls++
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"detail":{"code":"insufficient_credits","message":"This link needs 8 credits for its copies; 2 are available","estimated_credits":8,"available_credits":2}}`))
	})
	_, err := c.CreateLink(context.Background(), LinkCreate{AssetID: testLinkAsset, Name: "Board pack", Recipients: []string{"a@b.co"}})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 402 || apiErr.Code != "insufficient_credits" || calls != 1 {
		t.Fatalf("%v after %d calls", err, calls)
	}
}

func TestInvalidLinkInputIsRefusedBeforeAnyRequest(t *testing.T) {
	c := linkServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) { t.Error("no request expected") })
	ctx := context.Background()
	base := LinkCreate{AssetID: testLinkAsset, Name: "Board pack", Recipients: []string{"a@b.co"}}
	cases := []func(*LinkCreate){
		func(l *LinkCreate) { l.Recipients = nil },
		func(l *LinkCreate) { l.AllowedDomains = []string{"b.co"} },
		func(l *LinkCreate) { l.AssetID = "ast_1" },
		func(l *LinkCreate) { l.Name = " " },
		func(l *LinkCreate) { l.Name = strings.Repeat("📄", 201) },
		func(l *LinkCreate) { l.Prepare = "later" },
		func(l *LinkCreate) { l.DownloadLimit = 101 },
		func(l *LinkCreate) { l.WebhookID = "wh_1" },
		func(l *LinkCreate) { l.IdempotencyKey = "short" },
	}
	for i, change := range cases {
		l := base
		change(&l)
		if _, err := c.CreateLink(ctx, l); err == nil {
			t.Errorf("case %d: no error", i)
		}
	}
	long := base
	long.Name = strings.Repeat("📄", 200)
	if _, err := linkBody(long); err != nil {
		t.Error("200 characters should pass:", err)
	}
	for _, err := range []error{
		func() error { _, err := c.GetLink(ctx, "lnk_1"); return err }(),
		func() error { _, err := c.ListLinks(ctx, 51, ""); return err }(),
		func() error { _, err := c.ListLinks(ctx, 0, testLinkEvent); return err }(),
		func() error { _, err := c.ListLinkEvents(ctx, testLink, 101, ""); return err }(),
		func() error { _, err := c.ListLinkEvents(ctx, testLink, 0, testLink); return err }(),
	} {
		if err == nil {
			t.Error("expected an error")
		}
	}
}

func TestLinkRoutes(t *testing.T) {
	c := linkServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		route := r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
		switch route {
		case "POST /links/estimate?":
			if body["recipients"] == nil {
				t.Error("estimate body", body)
			}
			_, _ = w.Write([]byte(`{"estimated_credits":8,"enough_credits":true}`))
		case "GET /links?before=" + testLink + "&limit=5":
			_, _ = w.Write([]byte(`{"data":[{"id":"` + testLink + `"}],"next_cursor":"` + testLink + `"}`))
		case "GET /links/" + testLink + "?":
			_, _ = w.Write([]byte(`{"id":"` + testLink + `","recipients":[{"email":"alex@example.com","status":"opened","copy":{"status":"ready","asset_id":"` + testLinkAsset + `"}}]}`))
		case "GET /links/" + testLink + "/events?before=" + testLinkEvent:
			_, _ = w.Write([]byte(`{"data":[{"id":"` + testLinkEvent + `","type":"opened"}],"next_cursor":null}`))
		case "POST /links/" + testLink + "/revoke?":
			_, _ = w.Write([]byte(`{"id":"` + testLink + `","status":"revoked"}`))
		default:
			t.Error("unexpected", route)
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	estimate, err := c.EstimateLink(ctx, LinkCreate{AssetID: testLinkAsset, Name: "Board pack", Recipients: []string{"a@b.co"}})
	if err != nil || !estimate.EnoughCredits || *estimate.EstimatedCredits != 8 {
		t.Fatal(estimate, err)
	}
	page, err := c.ListLinks(ctx, 5, testLink)
	if err != nil || *page.NextCursor != testLink || page.Data[0].ID != testLink {
		t.Fatal(page, err)
	}
	link, err := c.GetLink(ctx, testLink)
	if err != nil || link.Recipients[0].Copy.Status != "ready" || *link.Recipients[0].Copy.AssetID != testLinkAsset {
		t.Fatal(link, err)
	}
	events, err := c.ListLinkEvents(ctx, testLink, 0, testLinkEvent)
	if err != nil || events.Data[0].Type != "opened" || events.NextCursor != nil {
		t.Fatal(events, err)
	}
	if revoked, err := c.RevokeLink(ctx, testLink); err != nil || revoked.Status != "revoked" {
		t.Fatal(revoked, err)
	}
}

func TestDetectionNamesTheCopyOrAsset(t *testing.T) {
	id := strings.Repeat("ab", 32)
	bodies := []string{
		`{"watermarked":true,"confidence":0.9,"watermark_id":"` + id + `","copy":{"asset_id":"` + testLinkAsset + `","link":{"id":"` + testLink + `","name":"Board pack"},"recipient":"alex@example.com","opened_at":null}}`,
		`{"watermarked":true,"confidence":0.9,"watermark_id":"` + id + `","asset":{"asset_id":"` + testLinkAsset + `","name":"a.png","metadata":{},"created_at":"2026-10-27T00:00:00Z"}}`,
		`{"watermarked":true,"confidence":0.9,"watermark_id":"` + id + `"}`,
	}
	var results []*DetectionResult
	for _, body := range bodies {
		d, err := detection([]byte(body), http.Header{})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, d)
	}
	if c := results[0].Copy; c == nil || c.AssetID == nil || *c.AssetID != testLinkAsset || c.Link.ID != testLink || c.Recipient != "alex@example.com" || c.OpenedAt != nil || results[0].Asset != nil {
		t.Fatalf("%+v", results[0])
	}
	if results[1].Copy != nil || results[1].Asset == nil || results[1].Asset.Name != "a.png" {
		t.Fatalf("%+v", results[1])
	}
	if results[2].Copy != nil || results[2].Asset != nil {
		t.Fatalf("%+v", results[2])
	}
}
