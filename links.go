package etchv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Traceable links share a source asset so that each recipient opens their own
// watermarked copy. See https://etchv.com/docs/api/links.

// LinkCreate describes a link for CreateLink and EstimateLink. Give exactly one
// of Recipients and AllowedDomains; zero values of the other fields are omitted.
type LinkCreate struct {
	// AssetID is a source asset (kind "source").
	AssetID string
	// Name is 1–200 characters, shown to recipients.
	Name string
	// Recipients are recipients' emails, up to 100.
	Recipients []string
	// AllowedDomains are email domains whose addresses may open the link
	// (exact match), up to 10.
	AllowedDomains []string
	// ExpiresAt defaults to 30 days from now, or the source file's expiry if
	// sooner; it must be at least five minutes away and at most a year.
	ExpiresAt time.Time
	// Prepare is "on_open" or "on_create"; empty lets the API choose.
	Prepare string
	// DownloadLimit is downloads allowed per recipient, 1–100; 0 is unlimited.
	DownloadLimit int
	// WebhookID receives link.opened and link.locked events.
	WebhookID string
	// IdempotencyKey makes CreateLink safely repeatable: 8–128 letters,
	// digits, hyphens or underscores. CreateLink generates one when empty.
	IdempotencyKey string
}

// LinkCopy is a recipient's watermarked copy.
type LinkCopy struct {
	// Status is "not_started", "preparing", "ready" or "failed".
	Status string `json:"status"`
	// AssetID is the copy in the asset library, once ready.
	AssetID   *string `json:"asset_id"`
	ErrorCode *string `json:"error_code"`
}

// LinkRecipient is one recipient of a link, with their opens and downloads.
type LinkRecipient struct {
	// ID ("lrc_…") is what [Client.RemoveLinkRecipient] takes.
	ID    string `json:"id"`
	Email string `json:"email"`
	// Status is "not_opened" or "opened".
	Status string `json:"status"`
	// OpenedAt is the first open, after the recipient confirmed their email.
	OpenedAt      *string  `json:"opened_at"`
	LastOpenedAt  *string  `json:"last_opened_at"`
	OpenCount     int      `json:"open_count"`
	DownloadCount int      `json:"download_count"`
	Copy          LinkCopy `json:"copy"`
}

// Link is a traceable link. Timestamps are RFC 3339 strings; nullable fields
// are pointers. Recipients is empty in ListLinks.
type Link struct {
	ID string `json:"id"`
	// URL is what you send to your recipients.
	URL       string `json:"url"`
	Name      string `json:"name"`
	AssetID   string `json:"asset_id"`
	MediaType string `json:"media_type"`
	// Status is "active", "revoked", "expired" or "unavailable" (the source
	// file was deleted or expired, so no new copies are made).
	Status           string          `json:"status"`
	Prepare          string          `json:"prepare"`
	AllowedDomains   []string        `json:"allowed_domains"`
	ExpiresAt        string          `json:"expires_at"`
	DownloadLimit    *int            `json:"download_limit"`
	RecipientCount   int             `json:"recipient_count"`
	CreditsPerCopy   int             `json:"credits_per_copy"`
	EstimatedCredits *int            `json:"estimated_credits"`
	BatchID          *string         `json:"batch_id"`
	WebhookID        *string         `json:"webhook_id"`
	CreatedAt        string          `json:"created_at"`
	RevokedAt        *string         `json:"revoked_at"`
	Recipients       []LinkRecipient `json:"recipients"`
}

// LinkPage is one page of links, newest first. Pass NextCursor as before to
// ListLinks for the next page; nil means the last page.
type LinkPage struct {
	Data       []Link  `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// LinkEstimate is what a link's copies would cost, from EstimateLink.
type LinkEstimate struct {
	AssetID        string `json:"asset_id"`
	MediaType      string `json:"media_type"`
	Prepare        string `json:"prepare"`
	CreditsPerCopy int    `json:"credits_per_copy"`
	// EstimatedCredits is nil for allowed domains.
	EstimatedCredits *int `json:"estimated_credits"`
	AvailableCredits int  `json:"available_credits"`
	// EnoughCredits reports whether creating the link now would pass the credit check.
	EnoughCredits   bool   `json:"enough_credits"`
	LatestExpiresAt string `json:"latest_expires_at"`
}

// LinkEvent is an open, download or lock of a link.
type LinkEvent struct {
	ID string `json:"id"`
	// Type is "opened", "downloaded" or "locked" (too many wrong codes).
	Type string `json:"type"`
	// Recipient is nil for "locked".
	Recipient  *string `json:"recipient"`
	CreatedAt  string  `json:"created_at"`
	VerifiedBy *string `json:"verified_by"`
	// AssetID is the copy downloaded, for "downloaded".
	AssetID *string `json:"asset_id"`
}

// LinkEventPage is one page of a link's events, newest first.
type LinkEventPage struct {
	Data       []LinkEvent `json:"data"`
	NextCursor *string     `json:"next_cursor"`
}

// DetectedCopy names the traceable link copy a detected watermark is on.
type DetectedCopy struct {
	// AssetID is the recipient's copy in the asset library.
	AssetID   *string      `json:"asset_id"`
	Link      DetectedLink `json:"link"`
	Recipient string       `json:"recipient"`
	// OpenedAt is when the recipient first opened the link, or nil.
	OpenedAt *string `json:"opened_at"`
}

// DetectedLink is the link a detected copy was made for.
type DetectedLink struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DetectedAsset names another of your watermarked assets a detected watermark is on.
type DetectedAsset struct {
	AssetID   string         `json:"asset_id"`
	Name      string         `json:"name"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt string         `json:"created_at"`
}

var (
	linkID      = regexp.MustCompile(`^lnk_[a-f0-9]{32}$`)
	linkEventID = regexp.MustCompile(`^lev_[a-f0-9]{32}$`)
	recipientID = regexp.MustCompile(`^lrc_[a-f0-9]{32}$`)
)

func linkPath(id string) (string, error) {
	if !linkID.MatchString(id) {
		return "", errors.New("etchv: invalid link ID")
	}
	return "links/" + id, nil
}

func linkBody(l LinkCreate) (map[string]any, error) {
	if !assetID.MatchString(l.AssetID) {
		return nil, errors.New("etchv: invalid asset ID")
	}
	if strings.TrimSpace(l.Name) == "" || utf8.RuneCountInString(l.Name) > 200 {
		return nil, errors.New("etchv: name must be 1–200 characters")
	}
	if (len(l.Recipients) == 0) == (len(l.AllowedDomains) == 0) {
		return nil, errors.New("etchv: give either Recipients or AllowedDomains")
	}
	body := map[string]any{"asset_id": l.AssetID, "name": l.Name}
	if len(l.Recipients) > 0 {
		body["recipients"] = l.Recipients
	} else {
		body["allowed_domains"] = l.AllowedDomains
	}
	if !l.ExpiresAt.IsZero() {
		body["expires_at"] = l.ExpiresAt.Format(time.RFC3339Nano)
	}
	switch l.Prepare {
	case "":
	case "on_open", "on_create":
		body["prepare"] = l.Prepare
	default:
		return nil, errors.New(`etchv: prepare must be "on_open" or "on_create"`)
	}
	if l.DownloadLimit < 0 || l.DownloadLimit > 100 {
		return nil, errors.New("etchv: download limit must be between 1 and 100")
	}
	if l.DownloadLimit > 0 {
		body["download_limit"] = l.DownloadLimit
	}
	if l.WebhookID != "" {
		if !webhookID.MatchString(l.WebhookID) {
			return nil, errors.New("etchv: invalid webhook ID")
		}
		body["webhook_id"] = l.WebhookID
	}
	return body, nil
}

// CreateLink shares a source asset so each recipient downloads their own
// watermarked copy (links:write). Send the returned Link.URL yourself. A
// workspace without the credits for the copies gets an [*Error] with
// StatusCode 402 and Code "insufficient_credits". Retries reuse one
// Idempotency-Key, so a retry returns the same link.
func (c *Client) CreateLink(ctx context.Context, l LinkCreate) (*Link, error) {
	body, err := linkBody(l)
	if err != nil {
		return nil, err
	}
	key := l.IdempotencyKey
	if key == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		key = hex.EncodeToString(b[:])
	} else if !idempotencyKey.MatchString(key) {
		return nil, errors.New("etchv: idempotency key must contain 8–128 letters, digits, hyphens or underscores")
	}
	var link Link
	if err := c.jsonCall(ctx, call{method: "POST", path: "links", idempotencyKey: key, retry: true}, body, &link); err != nil {
		return nil, err
	}
	return &link, nil
}

// EstimateLink runs every check of CreateLink without creating anything and
// reports the credits its copies would take (links:write).
func (c *Client) EstimateLink(ctx context.Context, l LinkCreate) (*LinkEstimate, error) {
	body, err := linkBody(l)
	if err != nil {
		return nil, err
	}
	var estimate LinkEstimate
	if err := c.json(ctx, "POST", "links/estimate", nil, body, &estimate); err != nil {
		return nil, err
	}
	return &estimate, nil
}

// ListLinks lists links newest first, without recipients (links:read). limit
// is 1–50 (0 uses the API default, 20); before is the previous page's NextCursor.
func (c *Client) ListLinks(ctx context.Context, limit int, before string) (*LinkPage, error) {
	q, err := pageQuery(limit, 50, before, linkID, "link cursor")
	if err != nil {
		return nil, err
	}
	var page LinkPage
	if err := c.json(ctx, "GET", "links", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// GetLink reads a link with each recipient and their copy (links:read).
func (c *Client) GetLink(ctx context.Context, id string) (*Link, error) {
	path, err := linkPath(id)
	if err != nil {
		return nil, err
	}
	var link Link
	if err := c.json(ctx, "GET", path, nil, nil, &link); err != nil {
		return nil, err
	}
	return &link, nil
}

// ListLinkEvents lists a link's opens, downloads and locks newest first
// (links:read). limit is 1–100 (0 uses the API default, 50).
func (c *Client) ListLinkEvents(ctx context.Context, id string, limit int, before string) (*LinkEventPage, error) {
	path, err := linkPath(id)
	if err != nil {
		return nil, err
	}
	q, err := pageQuery(limit, 100, before, linkEventID, "link event cursor")
	if err != nil {
		return nil, err
	}
	var page LinkEventPage
	if err := c.json(ctx, "GET", path+"/events", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// RevokeLink stops new opens and downloads at once (links:write). Copies not
// yet queued are canceled; calling again returns the link.
func (c *Client) RevokeLink(ctx context.Context, id string) (*Link, error) {
	path, err := linkPath(id)
	if err != nil {
		return nil, err
	}
	var link Link
	if err := c.json(ctx, "POST", path+"/revoke", nil, nil, &link); err != nil {
		return nil, err
	}
	return &link, nil
}

// DeleteLink deletes a link with every recipient's opens, downloads and copy
// (links:write and assets:delete, owner or admin). The link stops at once, as
// on a revoke. This can't be undone, and detection no longer names the link's
// recipients. Calling again also succeeds.
func (c *Client) DeleteLink(ctx context.Context, id string) error {
	path, err := linkPath(id)
	if err != nil {
		return err
	}
	return c.json(ctx, "DELETE", path, nil, nil, nil)
}

// RemoveLinkRecipient deletes one recipient's opens, downloads and copy, for
// example when they ask to be erased, and returns the link without them
// (links:write and assets:delete, owner or admin). This can't be undone, and
// detection no longer names the recipient. recipient is the recipient's
// [LinkRecipient.ID].
func (c *Client) RemoveLinkRecipient(ctx context.Context, id, recipient string) (*Link, error) {
	path, err := linkPath(id)
	if err != nil {
		return nil, err
	}
	if !recipientID.MatchString(recipient) {
		return nil, errors.New("etchv: invalid recipient ID")
	}
	var link Link
	if err := c.json(ctx, "DELETE", path+"/recipients/"+recipient, nil, nil, &link); err != nil {
		return nil, err
	}
	return &link, nil
}

func pageQuery(limit, most int, before string, cursor *regexp.Regexp, label string) (url.Values, error) {
	q := url.Values{}
	if limit < 0 || limit > most {
		return nil, errors.New("etchv: limit must be between 1 and " + strconv.Itoa(most))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before != "" {
		if !cursor.MatchString(before) {
			return nil, errors.New("etchv: invalid " + label)
		}
		q.Set("before", before)
	}
	return q, nil
}
