# Etchv Go SDK

Official server-side client for image, PDF and video watermarking. Go 1.25+. MIT licensed.

## Install

```sh
go get github.com/etchv-labs/go-sdk@v0.3.0
```

## Example

Set `ETCHV_API_KEY` in your environment.

```go
package main

import (
    "context"
    "os"
    "github.com/etchv-labs/go-sdk"
)

func main() {
    client, err := etchv.New(os.Getenv("ETCHV_API_KEY"))
    if err != nil { panic(err) }
    file, err := os.ReadFile("photo.jpg")
    if err != nil { panic(err) }
    result, err := client.EmbedImage(context.Background(), file,
        map[string]any{"asset": "photo-123"}, etchv.Options{Filename: "photo.jpg"})
    if err != nil { panic(err) }
    if err := os.WriteFile(result.Filename, result.Bytes, 0600); err != nil { panic(err) }
}
```

## Methods

`EmbedImage` / `DetectImage`, `EmbedDocument` / `DetectDocument`, `EmbedVideo` / `DetectVideo`. Each embed method takes file bytes, JSON metadata and request options; detection takes bytes and options.

Resume saved jobs with `GetEmbedResult(ctx, requestID)` / `GetDetectionResult(ctx, requestID)`.

`NewWithOptions(key, baseURL, timeout)`; all calls accept a context for cancellation. Inspect `*etchv.Error` with `errors.As` for `StatusCode`, `Detail`, `RequestID`, and `IdempotencyKey`.

## Supported formats

| Media | Formats | Preservation |
| --- | --- | --- |
| Images | PNG, JPEG/JPG, WebP, GIF, TIFF/TIF, BMP, PPM, PSD, PSB | Original format, supported animation, TIFF pages and PSD/PSB layers |
| Documents | PDF | Selectable text, vector content, page sizes and supported links |
| Video | MP4, MOV with supported H.264 video | Container, frame timing and supported AAC audio |

Send encoded file bytes and the original filename. The SDK does not convert or flatten files.
Embedding returns native bytes, MIME type, filename, watermark ID and request ID. Save the
returned bytes directly. Detection returns `watermarked`, confidence, a nullable watermark
ID, request ID and per-unit results for frames/pages/composites. A top-level watermark ID
requires all units to agree. Forensic `data` must be a non-empty JSON object; detection
recovers its SHA-256 identifier, not the original data.

The SDK supports the API's **qualified profiles**, not every possible file with these extensions:

- Files: at most 20 MB; the dashboard's separate 4 MB limit does not apply to SDK calls.
- Images: see [image limits](https://etchv.com/docs/api/embed) for bit depth, frame/page and editable-layer limits.
- PDF: up to 8 pages, 4 million pixels/page and 16 million total at 144 dpi. Encrypted, signed,
  form-containing, rotated and active-content PDFs are outside this profile.
  See [PDF requirements](https://etchv.com/docs/api/documents).
- Video: at most 120 seconds, 240 frames, 1 million pixels/frame and 40 million total;
  all limits apply together. Progressive 8-bit H.264, constant 1–60 fps, even dimensions,
  square pixels, no rotation or HDR. Optional synchronized mono/stereo AAC-LC audio is copied,
  **not watermarked**. Lossless H.264 output requires a compatible decoder and can increase
  file size. See [video requirements](https://etchv.com/docs/api/videos).
- DOCX, PPTX, AVI and other codecs remain planned; these SDKs do not claim support for them.

## Durability, billing and errors

The production base URL defaults to `https://api.etchv.com`. Keep API keys on your server.
Keys require `watermarks:embed` or `watermarks:detect` scopes and available credits.
Each successful image/PDF operation costs one credit per file. Each successful video
embedding or detection costs **one credit per started minute**.

Embedding and video detection generate an idempotency key, retry transient network failures
and HTTP 429/502/503/504, and poll pending jobs. Explicitly failed jobs are not retried.
Image/PDF detection is synchronous and is not automatically retried. Redirects are rejected;
polling paths are built from validated request IDs, never from server-provided URLs.

The default client deadline is 120 seconds. Timeout or client cancellation does not cancel
server work. Supply and persist your own idempotency key in the request options before a call
if you need recovery across process restarts. Retry with the same key, bytes, metadata and
operation to retrieve the same job without another charge. Saved results last 24 hours.
Errors with status 0 indicate a client/transport failure; deadline errors include recovery
identifiers when known. HTTP 401/403 indicates auth/scopes, 402 credits/billing, 409 a conflicting
idempotency key, and 422 an invalid or unsupported media profile.

## Development

Tests are written in the SDK’s own language and run with its standard test toolchain.
The tests use synthetic file signatures to check transport and protocol behavior across all
formats; the API repository separately tests actual watermark quality and media preservation.

```sh
go test -race ./...
```

This public repository is synchronized from the Etchv development monorepo. Issues and pull
requests are welcome; maintainers incorporate accepted changes into the source before publishing
the next snapshot. The MIT license covers this SDK, not the hosted service.

## Asset library

New successful embeddings save original and verified output assets. Files remain
downloadable for 30 days; records stay until deleted. Use `assets:read` for listing,
inspection and downloads, `assets:write` for edits, and `assets:delete` with current
owner/admin membership for deletion. Existing keys need replacement to add scopes.

```go
page, err := client.ListAssets(ctx, etchv.AssetListOptions{Kind: "watermarked", Limit: 25})
if err != nil { return err }
for _, item := range page.Items {
    asset, err := client.GetAsset(ctx, item.ID)
    if err != nil { return err }
    _, err = client.UpdateAsset(ctx, asset.ID, asset.Version,
        map[string]any{"metadata": map[string]any{"campaign": "spring"}})
    if err != nil { return err }
}
// DownloadAsset(ctx, id) returns file bytes. NextCursor continues the same filters.
```

Edits require the current version; reload and reconcile on HTTP 409. Metadata is
replaced, not merged, and does not change the embedded watermark. Asset operations
consume no credits. Downloads require authentication and return the original file
format. Single and bulk deletion methods are also available; batches contain at
most 50 IDs and delete atomically. Deleting an output blocks its job result replay.
See [the asset API](https://etchv.com/docs/api/assets) for the complete contract.

## Async jobs and webhooks

Submit a background job and receive a JSON receipt without polling automatically. Choose `images`, `documents`, or `videos`; every currently supported native format uses the same submission method.

```go
job, err := client.SubmitEmbed(ctx, "documents", pdfBytes,
    map[string]any{"delivery": "delivery_001"},
    etchv.Options{Filename: "document.pdf", IdempotencyKey: "delivery_001"}, webhookID)
if err != nil { return err }
status, err := client.GetJob(ctx, job["request_id"].(string), false)
```

Use the corresponding submission method for detection without forensic data. For detection status, set the status method’s `detect` argument to true. Existing embed/detect methods continue waiting for results.

Create an endpoint in the [Etchv dashboard](https://etchv.com/dashboard/webhooks), then pass its ID when submitting. Persist your idempotency key before the upload so a lost receipt can be recovered safely. Download from the authenticated result URL after success, or use the existing result method. See the [async guide](https://etchv.com/docs/api/async) and [webhook verification guide](https://etchv.com/docs/api/webhooks).
