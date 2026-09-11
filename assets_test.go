package etchv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestAssetProtocol(t *testing.T) {
	data, _ := os.ReadFile("tests/assets.json")
	var record Asset
	_ = json.Unmarshal(data, &record)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing authentication")
		}
		if r.URL.Query().Get("cursor") != "" {
			w.WriteHeader(409)
			return
		}
		switch r.Method {
		case "PATCH":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["version"] != float64(1) || body["name"] != "renamed" {
				t.Error("wrong patch")
			}
			a := record
			a.Version = 2
			_ = json.NewEncoder(w).Encode(a)
		case "DELETE":
			w.WriteHeader(204)
		case "POST":
			var body struct {
				IDs []string `json:"asset_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.IDs) != 1 || body.IDs[0] != record.ID {
				t.Error("wrong deletion")
			}
			w.WriteHeader(204)
		default:
			if r.URL.Path == "/assets" {
				if r.URL.Query().Get("kind") != "watermarked" {
					t.Error("filter lost")
				}
				next := "next-page"
				_ = json.NewEncoder(w).Encode(AssetPage{[]Asset{record}, &next})
			} else if r.URL.Path == "/assets/"+record.ID+"/content" {
				_, _ = w.Write([]byte("file"))
			} else {
				_, _ = w.Write(data)
			}
		}
	}))
	defer server.Close()
	c, _ := NewWithOptions("test-key", server.URL, time.Second)
	ctx := context.Background()
	page, err := c.ListAssets(ctx, AssetListOptions{Kind: "watermarked"})
	if err != nil || *page.NextCursor != "next-page" {
		t.Fatal(page, err)
	}
	a, err := c.GetAsset(ctx, record.ID)
	if err != nil || a.Metadata["campaign"] != "launch" {
		t.Fatal(a, err)
	}
	a, err = c.UpdateAsset(ctx, record.ID, 1, map[string]any{"name": "renamed"})
	if err != nil || a.Version != 2 {
		t.Fatal(a, err)
	}
	b, err := c.DownloadAsset(ctx, record.ID)
	if err != nil || string(b) != "file" {
		t.Fatal(err)
	}
	if err = c.DeleteAsset(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteAssets(ctx, []string{record.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = c.ListAssets(ctx, AssetListOptions{Cursor: "next-page"})
	if e, ok := err.(*Error); !ok || e.StatusCode != 409 {
		t.Fatal(err)
	}
	if _, err = c.GetAsset(ctx, "../other"); err == nil {
		t.Fatal("unsafe asset ID accepted")
	}
}
