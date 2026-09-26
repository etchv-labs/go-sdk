package etchv

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

// Asset is an asset library record for an original upload or a watermarked
// output. Timestamps are RFC 3339 strings; nullable fields are pointers.
type Asset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is "source" or "watermarked".
	Kind string `json:"kind"`
	// MediaType is "image", "document" or "video".
	MediaType     string  `json:"media_type"`
	Format        string  `json:"format"`
	ContentType   string  `json:"content_type"`
	SizeBytes     int64   `json:"size_bytes"`
	SHA256        string  `json:"sha256"`
	ParentAssetID *string `json:"parent_asset_id"`
	RequestID     string  `json:"request_id"`
	WatermarkID   *string `json:"watermark_id"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	// FileExpiresAt is nil for results stored in a customer bucket.
	FileExpiresAt *string `json:"file_expires_at"`
	FileAvailable bool    `json:"file_available"`
	// Version must be sent with UpdateAsset.
	Version int `json:"version"`
	// Metadata is nil in listings; fetch the record to read it.
	Metadata    map[string]any `json:"metadata"`
	DownloadURL *string        `json:"download_url"`
	// StorageProvider is "etchv", "s3", "gcs" or "azure".
	StorageProvider      string  `json:"storage_provider"`
	StorageStatus        string  `json:"storage_status"`
	StorageDestinationID *string `json:"storage_destination_id"`
	StorageDeliveryID    *string `json:"storage_delivery_id"`
	StagingExpiresAt     *string `json:"staging_expires_at"`
	StagingDeletedAt     *string `json:"staging_deleted_at"`
}

// AssetPage is one page of assets, newest first. Pass NextCursor in
// AssetListOptions.Cursor with the same filters to continue; nil means the
// last page.
type AssetPage struct {
	Items      []Asset `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// AssetListOptions filters ListAssets. Zero values are omitted.
type AssetListOptions struct {
	// Limit is 1–100 records per page; 0 uses the API default (25).
	Limit int
	// Cursor is NextCursor from the previous page.
	Cursor string
	// Kind is "source" or "watermarked".
	Kind string
	// MediaType is "image", "document" or "video".
	MediaType string
	// WatermarkID is an exact 64-character hexadecimal watermark ID.
	WatermarkID string
}

// AssetUpdate changes an asset's name and/or metadata. Version must equal the
// asset's current version; a stale version returns HTTP 409.
type AssetUpdate struct {
	Version int
	// Name is the new display name; empty leaves it unchanged.
	Name string
	// Metadata replaces (does not merge) the metadata when non-nil. Use an
	// empty map to clear it.
	Metadata map[string]any
}

var assetID = regexp.MustCompile(`^ast_[a-f0-9]{64}$`)

func assetPath(id string) (string, error) {
	if !assetID.MatchString(id) {
		return "", errors.New("etchv: invalid asset ID")
	}
	return "assets/" + id, nil
}

// ListAssets lists asset records (assets:read).
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
	if err := c.json(ctx, "GET", "assets", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// GetAsset reads an asset record, including metadata (assets:read).
func (c *Client) GetAsset(ctx context.Context, id string) (*Asset, error) {
	path, err := assetPath(id)
	if err != nil {
		return nil, err
	}
	var a Asset
	if err := c.json(ctx, "GET", path, nil, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateAsset renames an asset or replaces its metadata (assets:write) and
// returns the record with its incremented version.
func (c *Client) UpdateAsset(ctx context.Context, id string, update AssetUpdate) (*Asset, error) {
	path, err := assetPath(id)
	if err != nil {
		return nil, err
	}
	if update.Name == "" && update.Metadata == nil {
		return nil, errors.New("etchv: provide Name or Metadata to update")
	}
	body := map[string]any{"version": update.Version}
	if update.Name != "" {
		body["name"] = update.Name
	}
	if update.Metadata != nil {
		body["metadata"] = update.Metadata
	}
	var a Asset
	if err := c.json(ctx, "PATCH", path, nil, body, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteAsset deletes an asset (assets:delete, owner or admin).
func (c *Client) DeleteAsset(ctx context.Context, id string) error {
	path, err := assetPath(id)
	if err != nil {
		return err
	}
	return c.json(ctx, "DELETE", path, nil, nil, nil)
}

// DeleteAssets atomically deletes 1–50 assets (assets:delete, owner or admin).
func (c *Client) DeleteAssets(ctx context.Context, ids []string) error {
	if len(ids) < 1 || len(ids) > 50 {
		return errors.New("etchv: provide 1–50 asset IDs")
	}
	for _, id := range ids {
		if _, err := assetPath(id); err != nil {
			return err
		}
	}
	return c.json(ctx, "POST", "assets/bulk-delete", nil, map[string]any{"asset_ids": ids}, nil)
}

// DownloadAsset downloads an asset file in its original format (assets:read).
// An expired file returns an [*Error] with StatusCode 410.
func (c *Client) DownloadAsset(ctx context.Context, id string) (*Download, error) {
	path, err := assetPath(id)
	if err != nil {
		return nil, err
	}
	return c.download(ctx, path+"/content")
}

// StorageDestination is a customer cloud storage destination. It never
// contains stored credentials.
type StorageDestination struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Provider is "s3", "gcs" or "azure".
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	// Visibility is "private" or "public".
	Visibility          string  `json:"visibility"`
	Region              *string `json:"region"`
	RoleARN             *string `json:"role_arn"`
	Account             *string `json:"account"`
	ExternalID          string  `json:"external_id"`
	Enabled             bool    `json:"enabled"`
	VerifiedAt          *string `json:"verified_at"`
	CreatedAt           string  `json:"created_at"`
	LastError           *string `json:"last_error"`
	CredentialExpiresAt *string `json:"credential_expires_at"`
	// GCSAuth is "service_account_key" or "workload_identity" for GCS.
	GCSAuth                     *string `json:"gcs_auth"`
	GCSWorkloadIdentityProvider *string `json:"gcs_workload_identity_provider"`
	GCSServiceAccount           *string `json:"gcs_service_account"`
	// AWSPrincipalARN is the Etchv principal to trust in your cloud policy.
	AWSPrincipalARN *string `json:"aws_principal_arn"`
	// GCSSubject is the federated subject for keyless GCS destinations.
	GCSSubject *string `json:"gcs_subject"`
}

// StorageDestinationCreate describes a new destination. See the storage guide
// for the fields each provider requires. Its String and GoString methods
// redact Credentials.
type StorageDestinationCreate struct {
	Name string `json:"name"`
	// Provider is "s3", "gcs" or "azure".
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	// Prefix is the object key prefix; nil uses the API default ("etchv").
	Prefix *string `json:"prefix,omitempty"`
	// Visibility is "private" (default) or "public".
	Visibility string `json:"visibility,omitempty"`
	// Region and RoleARN are required for S3.
	Region  string `json:"region,omitempty"`
	RoleARN string `json:"role_arn,omitempty"`
	// Account is the Azure storage account name.
	Account string `json:"account,omitempty"`
	// Credentials is a GCS service account JSON key or an Azure container SAS.
	Credentials string `json:"credentials,omitempty"`
	// GCSAuth is "service_account_key" or "workload_identity".
	GCSAuth                     string `json:"gcs_auth,omitempty"`
	GCSWorkloadIdentityProvider string `json:"gcs_workload_identity_provider,omitempty"`
	GCSServiceAccount           string `json:"gcs_service_account,omitempty"`
}

// String describes the destination with Credentials redacted.
func (d StorageDestinationCreate) String() string {
	return fmt.Sprintf("StorageDestinationCreate{Name:%q Provider:%q Bucket:%q Credentials:%s}", d.Name, d.Provider, d.Bucket, redacted(d.Credentials))
}

// GoString is like String, for the %#v verb.
func (d StorageDestinationCreate) GoString() string { return d.String() }

// StorageDestinationUpdate enables or disables a destination or replaces its
// credentials. Nil/empty fields are unchanged. Its String and GoString
// methods redact Credentials.
type StorageDestinationUpdate struct {
	Enabled     *bool  `json:"enabled,omitempty"`
	Credentials string `json:"credentials,omitempty"`
}

// String describes the update with Credentials redacted.
func (u StorageDestinationUpdate) String() string {
	enabled := "unchanged"
	if u.Enabled != nil {
		enabled = fmt.Sprint(*u.Enabled)
	}
	return fmt.Sprintf("StorageDestinationUpdate{Enabled:%s Credentials:%s}", enabled, redacted(u.Credentials))
}

// GoString is like String, for the %#v verb.
func (u StorageDestinationUpdate) GoString() string { return u.String() }

func redacted(secret string) string {
	if secret == "" {
		return `""`
	}
	return "[REDACTED]"
}

// StorageDeliveryAttempt is one recorded upload attempt.
type StorageDeliveryAttempt struct {
	At        string  `json:"at"`
	Status    string  `json:"status"`
	ErrorCode *string `json:"error_code"`
}

// StorageDelivery tracks the upload of a watermarked asset to a destination.
type StorageDelivery struct {
	ID            string  `json:"id"`
	AssetID       string  `json:"asset_id"`
	RequestID     string  `json:"request_id"`
	DestinationID string  `json:"destination_id"`
	Provider      string  `json:"provider"`
	Key           string  `json:"key"`
	URI           *string `json:"uri"`
	PublicURL     *string `json:"public_url"`
	// Status is "queued", "uploading", "retrying", "stored", "failed" or "cancelled".
	Status        string                   `json:"status"`
	Attempts      int                      `json:"attempts"`
	CreatedAt     string                   `json:"created_at"`
	NextAttemptAt *string                  `json:"next_attempt_at"`
	CompletedAt   *string                  `json:"completed_at"`
	ErrorCode     *string                  `json:"error_code"`
	History       []StorageDeliveryAttempt `json:"history"`
	ExpiresAt     *string                  `json:"expires_at"`
}

// StorageDeliveryPage is one page of deliveries, newest first. Pass
// NextCursor to ListStorageDeliveries to continue; nil means the last page.
type StorageDeliveryPage struct {
	Items      []StorageDelivery `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

func destinationPath(id string) (string, error) {
	if !destinationID.MatchString(id) {
		return "", errors.New("etchv: invalid storage destination ID")
	}
	return "storage/destinations/" + id, nil
}

func deliveryPath(id string) (string, error) {
	if !deliveryID.MatchString(id) {
		return "", errors.New("etchv: invalid storage delivery ID")
	}
	return "storage/deliveries/" + id, nil
}

func (c *Client) destination(ctx context.Context, method, path string, body any) (*StorageDestination, error) {
	var d StorageDestination
	if err := c.json(ctx, method, path, nil, body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (c *Client) delivery(ctx context.Context, method, path string, body any) (*StorageDelivery, error) {
	var d StorageDelivery
	if err := c.json(ctx, method, path, nil, body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListStorageDestinations lists the organization's destinations (storage:read).
func (c *Client) ListStorageDestinations(ctx context.Context) ([]StorageDestination, error) {
	var list []StorageDestination
	if err := c.json(ctx, "GET", "storage/destinations", nil, nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// CreateStorageDestination connects a destination (storage:write, owner or
// admin). Verify it with VerifyStorageDestination before use.
func (c *Client) CreateStorageDestination(ctx context.Context, create StorageDestinationCreate) (*StorageDestination, error) {
	return c.destination(ctx, "POST", "storage/destinations", create)
}

// UpdateStorageDestination enables, disables or re-credentials a destination
// (storage:write). Replacing credentials requires verifying again.
func (c *Client) UpdateStorageDestination(ctx context.Context, id string, update StorageDestinationUpdate) (*StorageDestination, error) {
	path, err := destinationPath(id)
	if err != nil {
		return nil, err
	}
	return c.destination(ctx, "PATCH", path, update)
}

// DeleteStorageDestination disconnects a destination and discards its stored
// credentials (storage:write).
func (c *Client) DeleteStorageDestination(ctx context.Context, id string) error {
	path, err := destinationPath(id)
	if err != nil {
		return err
	}
	return c.json(ctx, "DELETE", path, nil, nil, nil)
}

// VerifyStorageDestination writes and reads back a connection probe
// (storage:write). A failed check returns an [*Error] with StatusCode 422.
func (c *Client) VerifyStorageDestination(ctx context.Context, id string) (*StorageDestination, error) {
	path, err := destinationPath(id)
	if err != nil {
		return nil, err
	}
	return c.destination(ctx, "POST", path+"/verify", nil)
}

// ListStorageDeliveries returns up to 50 deliveries for a destination, newest
// first (storage:read). Pass an empty cursor for the first page.
func (c *Client) ListStorageDeliveries(ctx context.Context, destination, cursor string) (*StorageDeliveryPage, error) {
	path, err := destinationPath(destination)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if cursor != "" {
		if !deliveryID.MatchString(cursor) {
			return nil, errors.New("etchv: invalid delivery cursor")
		}
		q.Set("after", cursor)
	}
	var page StorageDeliveryPage
	if err := c.json(ctx, "GET", path+"/deliveries", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// CreateStorageDelivery moves an Etchv-hosted watermarked asset to a verified
// destination (storage:write). key is an optional object key beneath the
// destination prefix. Repeating the call returns the existing delivery.
func (c *Client) CreateStorageDelivery(ctx context.Context, destination, asset, key string) (*StorageDelivery, error) {
	path, err := destinationPath(destination)
	if err != nil {
		return nil, err
	}
	if _, err := assetPath(asset); err != nil {
		return nil, err
	}
	body := map[string]string{"asset_id": asset}
	if key != "" {
		body["key"] = key
	}
	return c.delivery(ctx, "POST", path+"/deliveries", body)
}

// GetStorageDelivery reads a delivery's status (storage:read). It can return
// 404 until the watermark job that created it has succeeded.
func (c *Client) GetStorageDelivery(ctx context.Context, id string) (*StorageDelivery, error) {
	path, err := deliveryPath(id)
	if err != nil {
		return nil, err
	}
	return c.delivery(ctx, "GET", path, nil)
}

// RetryStorageDelivery requeues a failed or cancelled upload without another
// charge (storage:write).
func (c *Client) RetryStorageDelivery(ctx context.Context, id string) (*StorageDelivery, error) {
	path, err := deliveryPath(id)
	if err != nil {
		return nil, err
	}
	return c.delivery(ctx, "POST", path+"/retry", nil)
}

// DownloadStorageDelivery downloads a stored object through the authenticated
// API (storage:read). The delivery must have status "stored".
func (c *Client) DownloadStorageDelivery(ctx context.Context, id string) (*Download, error) {
	path, err := deliveryPath(id)
	if err != nil {
		return nil, err
	}
	return c.download(ctx, path+"/content")
}
