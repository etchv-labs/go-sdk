package etchv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
			if body["version"] != float64(1) || body["name"] != "renamed" || body["metadata"] == nil || len(body["metadata"].(map[string]any)) != 0 {
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
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("X-Asset-ID", record.ID)
				w.Header().Set("Content-Disposition", `attachment; filename="asset.png"; filename*=UTF-8''launch%20%C3%A9.png`)
				_, _ = w.Write([]byte("file"))
			} else {
				_, _ = w.Write(data)
			}
		}
	}))
	defer server.Close()
	c, _ := New("test-key", WithBaseURL(server.URL), WithTimeout(time.Second))
	ctx := context.Background()
	page, err := c.ListAssets(ctx, AssetListOptions{Kind: "watermarked"})
	if err != nil || *page.NextCursor != "next-page" {
		t.Fatal(page, err)
	}
	a, err := c.GetAsset(ctx, record.ID)
	if err != nil || a.Metadata["campaign"] != "launch" || a.FileExpiresAt != nil || a.StorageProvider != "s3" {
		t.Fatal(a, err)
	}
	a, err = c.UpdateAsset(ctx, record.ID, AssetUpdate{Version: 1, Name: "renamed", Metadata: map[string]any{}})
	if err != nil || a.Version != 2 {
		t.Fatal(a, err)
	}
	d, err := c.DownloadAsset(ctx, record.ID)
	if err != nil || string(d.Bytes) != "file" || d.ContentType != "image/png" || d.Filename != "launch é.png" || d.AssetID != record.ID {
		t.Fatal(d, err)
	}
	if _, err = c.UpdateAsset(ctx, record.ID, AssetUpdate{Version: 1}); err == nil {
		t.Fatal("empty update accepted")
	}
	if err = c.DeleteAsset(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteAssets(ctx, []string{record.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = c.ListAssets(ctx, AssetListOptions{Cursor: "next-page"})
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 409 {
		t.Fatal(err)
	}
	if _, err = c.GetAsset(ctx, "../other"); err == nil {
		t.Fatal("unsafe asset ID accepted")
	}
}

func TestStorageProtocol(t *testing.T) {
	dst := "dst_" + strings.Repeat("c", 32)
	std := "std_" + strings.Repeat("d", 64)
	ast := "ast_" + strings.Repeat("a", 64)
	destination := map[string]any{"id": dst, "name": "Exports", "provider": "azure", "bucket": "exports", "prefix": "", "visibility": "private",
		"region": nil, "account": "acct", "enabled": true, "verified_at": nil, "created_at": "2026-09-12T00:00:00Z", "external_id": "etchv-x"}
	delivery := map[string]any{"id": std, "asset_id": ast, "destination_id": dst, "provider": "azure", "key": "reports/a.pdf", "status": "queued",
		"attempts": 0, "history": []any{map[string]any{"at": "2026-09-12T00:00:00Z", "status": "failed", "error_code": "provider_unavailable"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reply := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.Method + " " + r.URL.Path {
		case "GET /storage/destinations":
			reply(200, []any{destination})
		case "POST /storage/destinations":
			if body["credentials"] != "sv=2025&sig=secret" || body["prefix"] != "" || body["region"] != nil {
				t.Error("wrong create body", body)
			}
			reply(422, map[string]any{"detail": []any{map[string]any{"loc": []string{"body", "credentials"}, "msg": "Invalid storage field", "type": "value_error"}}})
		case "PATCH /storage/destinations/" + dst:
			if body["enabled"] != false || len(body) != 1 {
				t.Error("wrong update body", body)
			}
			reply(200, destination)
		case "DELETE /storage/destinations/" + dst:
			w.WriteHeader(204)
		case "POST /storage/destinations/" + dst + "/verify":
			reply(200, destination)
		case "GET /storage/destinations/" + dst + "/deliveries":
			if r.URL.Query().Get("after") != std {
				t.Error("cursor lost")
			}
			reply(200, map[string]any{"items": []any{delivery}, "next_cursor": std})
		case "POST /storage/destinations/" + dst + "/deliveries":
			if body["asset_id"] != ast || body["key"] != "reports/a.pdf" {
				t.Error("wrong delivery body", body)
			}
			reply(202, delivery)
		case "GET /storage/deliveries/" + std:
			reply(200, delivery)
		case "POST /storage/deliveries/" + std + "/retry":
			reply(202, delivery)
		case "GET /storage/deliveries/" + std + "/content":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="../../etc/passwd"`)
			_, _ = w.Write([]byte("%PDF-"))
		default:
			reply(404, map[string]any{"detail": "Storage delivery not found"})
		}
	}))
	defer server.Close()
	c, _ := New("test-key", WithBaseURL(server.URL), WithTimeout(time.Second))
	ctx := context.Background()
	list, err := c.ListStorageDestinations(ctx)
	if err != nil || len(list) != 1 || list[0].ID != dst || *list[0].Account != "acct" || list[0].Region != nil {
		t.Fatal(list, err)
	}
	empty := ""
	create := StorageDestinationCreate{Name: "Exports", Provider: "azure", Bucket: "exports", Prefix: &empty, Account: "acct", Credentials: "sv=2025&sig=secret"}
	_, err = c.CreateStorageDestination(ctx, create)
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 422 || strings.Contains(err.Error(), "secret") || !strings.Contains(e.Detail, "Invalid storage field") {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if strings.Contains(fmt.Sprintf(format, create), "secret") || strings.Contains(fmt.Sprintf(format, StorageDestinationUpdate{Credentials: "secret"}), "secret") {
			t.Fatal("credentials printed with", format)
		}
	}
	disabled := false
	if _, err = c.UpdateStorageDestination(ctx, dst, StorageDestinationUpdate{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.VerifyStorageDestination(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteStorageDestination(ctx, dst); err != nil {
		t.Fatal(err)
	}
	page, err := c.ListStorageDeliveries(ctx, dst, std)
	if err != nil || len(page.Items) != 1 || *page.NextCursor != std || *page.Items[0].History[0].ErrorCode != "provider_unavailable" {
		t.Fatal(page, err)
	}
	sd, err := c.CreateStorageDelivery(ctx, dst, ast, "reports/a.pdf")
	if err != nil || sd.ID != std || sd.Status != "queued" {
		t.Fatal(sd, err)
	}
	if sd, err = c.GetStorageDelivery(ctx, std); err != nil || sd.Key != "reports/a.pdf" {
		t.Fatal(sd, err)
	}
	if sd, err = c.RetryStorageDelivery(ctx, std); err != nil || sd.AssetID != ast {
		t.Fatal(sd, err)
	}
	d, err := c.DownloadStorageDelivery(ctx, std)
	if err != nil || string(d.Bytes) != "%PDF-" || d.Filename != "" || d.ContentType != "application/pdf" {
		t.Fatal("unsafe filename or content", d, err)
	}
	for _, bad := range []error{
		c.DeleteStorageDestination(ctx, "../x"),
		func() error { _, err := c.GetStorageDelivery(ctx, "std_1"); return err }(),
		func() error { _, err := c.CreateStorageDelivery(ctx, dst, "ast_1", ""); return err }(),
		func() error { _, err := c.ListStorageDeliveries(ctx, dst, "bad"); return err }(),
	} {
		if bad == nil {
			t.Fatal("unsafe identifier accepted")
		}
	}
}
