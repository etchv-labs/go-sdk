package etchv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
)

type Asset struct {
	StorageProvider      string         `json:"storage_provider"`
	StorageStatus        string         `json:"storage_status"`
	StorageDestinationID *string        `json:"storage_destination_id"`
	StorageDeliveryID    *string        `json:"storage_delivery_id"`
	StagingExpiresAt     *string        `json:"staging_expires_at"`
	StagingDeletedAt     *string        `json:"staging_deleted_at"`
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	Kind                 string         `json:"kind"`
	MediaType            string         `json:"media_type"`
	Format               string         `json:"format"`
	ContentType          string         `json:"content_type"`
	SizeBytes            int64          `json:"size_bytes"`
	SHA256               string         `json:"sha256"`
	ParentAssetID        *string        `json:"parent_asset_id"`
	RequestID            string         `json:"request_id"`
	WatermarkID          *string        `json:"watermark_id"`
	CreatedAt            string         `json:"created_at"`
	UpdatedAt            string         `json:"updated_at"`
	FileExpiresAt        *string        `json:"file_expires_at"`
	FileAvailable        bool           `json:"file_available"`
	Version              int            `json:"version"`
	Metadata             map[string]any `json:"metadata"`
	DownloadURL          *string        `json:"download_url"`
}
type AssetPage struct {
	Items      []Asset `json:"items"`
	NextCursor *string `json:"next_cursor"`
}
type AssetListOptions struct {
	Limit                                int
	Cursor, Kind, MediaType, WatermarkID string
}

var assetID = regexp.MustCompile(`^ast_[a-f0-9]{64}$`)

func assetPath(id string) (string, error) {
	if !assetID.MatchString(id) {
		return "", fmt.Errorf("invalid asset ID")
	}
	return "assets/" + id, nil
}
func (c *Client) assetJSON(ctx context.Context, path, method string, body any, result any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	data, _, err := c.request(ctx, path, method, b, "application/json", "", false, false)
	if err != nil {
		return err
	}
	if result != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}
func (c *Client) ListAssets(ctx context.Context, opts AssetListOptions) (*AssetPage, error) {
	q := url.Values{}
	if opts.Limit != 0 {
		q.Set("limit", fmt.Sprint(opts.Limit))
	}
	for k, v := range map[string]string{"cursor": opts.Cursor, "kind": opts.Kind, "media_type": opts.MediaType, "watermark_id": opts.WatermarkID} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var page AssetPage
	err := c.assetJSON(ctx, "assets?"+q.Encode(), "GET", nil, &page)
	return &page, err
}
func (c *Client) GetAsset(ctx context.Context, id string) (*Asset, error) {
	path, err := assetPath(id)
	if err != nil {
		return nil, err
	}
	var a Asset
	err = c.assetJSON(ctx, path, "GET", nil, &a)
	return &a, err
}
func (c *Client) UpdateAsset(ctx context.Context, id string, version int, changes map[string]any) (*Asset, error) {
	path, err := assetPath(id)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	for k, v := range changes {
		body[k] = v
	}
	body["version"] = version
	var a Asset
	err = c.assetJSON(ctx, path, "PATCH", body, &a)
	return &a, err
}
func (c *Client) DeleteAsset(ctx context.Context, id string) error {
	path, err := assetPath(id)
	if err != nil {
		return err
	}
	return c.assetJSON(ctx, path, "DELETE", nil, nil)
}
func (c *Client) DeleteAssets(ctx context.Context, ids []string) error {
	if len(ids) < 1 || len(ids) > 50 {
		return fmt.Errorf("provide 1–50 asset IDs")
	}
	for _, id := range ids {
		if _, err := assetPath(id); err != nil {
			return err
		}
	}
	return c.assetJSON(ctx, "assets/bulk-delete", "POST", map[string]any{"asset_ids": ids}, nil)
}
func (c *Client) DownloadAsset(ctx context.Context, id string) ([]byte, error) {
	path, err := assetPath(id)
	if err != nil {
		return nil, err
	}
	b, _, err := c.request(ctx, path+"/content", "GET", nil, "", "", false, false)
	return b, err
}
