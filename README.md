# Etchv Go SDK

Server-side Go client for [Etchv](https://etchv.com): embed and detect invisible forensic watermarks in images, PDFs and videos.

## Install

```sh
go get github.com/etchv-labs/go-sdk@v1.0.0
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
