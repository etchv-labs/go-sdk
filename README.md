# Etchv Go SDK

Server-side Go client for [Etchv](https://etchv.com): embed and detect invisible forensic watermarks in images, PDFs and videos.

## Install

```sh
go get github.com/etchv-labs/go-sdk@v1.2.0
```

Requires Go 1.25+. The package name is `etchv`; the client is safe for concurrent use.

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	etchv "github.com/etchv-labs/go-sdk"
)

func main() {
	ctx := context.Background()
	client, err := etchv.New(os.Getenv("ETCHV_API_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	file, err := os.ReadFile("photo.jpg")
	if err != nil {
		log.Fatal(err)
	}
	result, err := client.EmbedImage(ctx, file,
		map[string]any{"recipient": "customer-123"}, etchv.Options{Filename: "photo.jpg"})
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(result.Filename, result.Bytes, 0o600); err != nil {
		log.Fatal(err)
	}
	detected, err := client.DetectImage(ctx, result.Bytes, etchv.Options{Filename: result.Filename})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(detected.Watermarked, detected.Confidence)
}
```

## Async jobs

```go
job, err := client.SubmitEmbed(ctx, etchv.MediaDocuments, pdf,
	map[string]any{"delivery": "delivery_001"},
	etchv.Options{Filename: "report.pdf", IdempotencyKey: "delivery_001"})
if err != nil {
	return err
}
status, err := client.GetEmbedJob(ctx, job.RequestID) // or GetEmbedResult, which waits
```

Resending the same request with the same `Options.IdempotencyKey` returns the existing job without another charge.
Detection uses `SubmitDetection`, `GetDetectionJob` and `GetDetectionResult`.

## Large files

Image uploads are limited to 50 MB; PDF and video uploads to 20 MB. Detection
takes the files Etchv delivered: up to 192 MB for images, 64 MB for PDFs and
100 MB for video.

Files over 40 MB are uploaded once to a signed URL and then referenced by ID,
so the request never carries the file. This is automatic in every embed,
detect and submit method; retries reuse the same upload. Image and PDF
detection above 95 MB runs as a background job and the call waits for it.
Change the threshold with `etchv.WithLargeFileThreshold`, or upload explicitly:

```go
upload, err := client.UploadFile(ctx, etchv.UploadDetect, delivered, "delivered.tiff")
if err != nil {
	return err
}
_ = upload.UploadID // send as the upload_id form field instead of file
```

## Many files at once

```go
items := []etchv.BatchItem{}
paths, _ := filepath.Glob("in/*.pdf")
for _, path := range paths {
	stem := strings.TrimSuffix(filepath.Base(path), ".pdf")
	items = append(items, etchv.BatchItem{Path: path, Data: map[string]any{"recipient": stem}})
}
batch, err := client.SubmitBatch(ctx, items, &etchv.BatchOptions{Archive: true})
if err != nil {
	return err
}
// BatchResults waits for the batch to finish, then yields each file in order.
for item, err := range client.BatchResults(ctx, batch.BatchID, 30*time.Minute) {
	switch {
	case err != nil:
		log.Printf("%s: %v", item.Filename, err)
	case item.OK():
		_ = os.WriteFile(filepath.Join("out", item.Result.Filename), item.Result.Bytes, 0o600)
	default:
		log.Printf("%s: %s (refunded)", item.Filename, item.ErrorCode)
	}
}
out, err := os.Create("results.zip") // every result plus manifest.json
if err != nil {
	return err
}
defer out.Close()
_, err = client.DownloadBatchArchiveTo(ctx, batch.BatchID, out, 30*time.Minute)
```

`SubmitBatch` takes up to 100 files, each with its own data, as bytes (`File`) or a path (`Path`). It creates the
batch, uploads each file straight to its own signed upload URL (four at a time; set
`BatchOptions.UploadConcurrency`), never sending your API key there, and starts it. Each batch carries an
`Idempotency-Key` (generated unless you set `BatchOptions.IdempotencyKey`): submitting again with the same key
returns the same batch and uploads only what it has not received. Files that are rejected or fail are refunded.
Upload URLs last 6 hours and a batch must start within 24 hours; results and the archive (with `Archive: true`,
up to 1 GB) stay available for 24 hours. `WaitForBatch`, `BatchResults` and the archive downloads wait up to their
timeout (one hour when zero), honoring `Retry-After` between polls; the archive download itself only fails when no
data arrives for the client timeout. `DownloadBatchArchive` returns the zip as bytes instead, and an archive that
cannot exist is an error with `Code` `batch_not_started`, `archive_not_requested`, `archive_too_large` or
`archive_unavailable`. Resubmitting a batch that expired before it started is an error; use a new key. Files already in one zip
of up to 55 MB can go to `SubmitBatchZip` instead. `GetBatch`, `CancelBatch` and `ListBatches` cover the rest.

## GPU processing

```go
result, err := client.EmbedVideo(ctx, video, data,
	etchv.Options{Filename: "clip.mp4", Accelerator: etchv.AcceleratorGPU})
fmt.Println(result.Accelerator) // "gpu", or "cpu" after an automatic fallback
```

`Options.Accelerator` (`AcceleratorCPU`, the default, or `AcceleratorGPU`) applies to every embed, detect and
submit method. GPU processing requires a Business plan or higher (HTTP 403 otherwise) and costs 3× credits. When
no GPU is ready the request runs on CPU at normal credits. `EmbedResult.Accelerator` and
`DetectionResult.Accelerator` report the hardware that actually ran; `Job` has `AcceleratorRequested` and
`Accelerator`.

Embedding, video detection, submissions and result downloads retry HTTP 429 and 502–504 within the client
timeout, honoring `Retry-After`; other calls return the `*etchv.Error`.

## Also included

- API key check: `CheckAPIKey`
- Batches: `SubmitBatch`, `SubmitBatchZip`, `GetBatch`, `WaitForBatch`, `BatchResults`, `DownloadBatchArchive`, `DownloadBatchArchiveTo`, `CancelBatch`, `ListBatches`
- Assets: `ListAssets`, `GetAsset`, `UpdateAsset`, `DeleteAsset`, `DeleteAssets`, `DownloadAsset`
- Webhooks: `ListWebhooks`, `CreateWebhook`, `UpdateWebhook`, `DeleteWebhook`, `ListWebhookDeliveries`, `RedeliverWebhook`; `etchv.VerifyWebhook` checks the signature, timestamp and `X-Etchv-Event-ID`
- Customer storage: `ListStorageDestinations`, `CreateStorageDestination`, `UpdateStorageDestination`, `DeleteStorageDestination`, `VerifyStorageDestination`, `ListStorageDeliveries`, `CreateStorageDelivery`, `GetStorageDelivery`, `RetryStorageDelivery`, `DownloadStorageDelivery`

## Errors

Every failure is an `*etchv.Error` with `StatusCode` (`0` for network failures and deadlines), `Detail`,
`RequestID` and `IdempotencyKey`. Include the request ID when contacting support.

```go
var apiErr *etchv.Error
if errors.As(err, &apiErr) {
	log.Printf("HTTP %d, request %s: %s", apiErr.StatusCode, apiErr.RequestID, apiErr.Detail)
}
```

Structured errors also set `Code`, `Message` (used as the error text) and `Limit`, for example
`rate_limited` or `concurrency_limited` on HTTP 429. A 429 error's `RetryAfter` is the wait the API asked for (zero when it sent none).

## Links

- Full guide: https://etchv.com/docs/sdks/go
- API reference: https://etchv.com/docs
- Support: hello@etchv.com

License: MIT.

Questions or bug reports: open an issue here or email hello@etchv.com.
