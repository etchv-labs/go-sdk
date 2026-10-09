package etchv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxBatchItems is the largest number of files in one batch.
const MaxBatchItems = 100

// MaxBatchZipSize is the largest zip [Client.SubmitBatchZip] sends (55 MB).
const MaxBatchZipSize = 55 * 1024 * 1024

// MaxBatchArchiveSize is the largest batch archive [Client.DownloadBatchArchive]
// reads: the API's 1 GB of results plus room for the zip's own structure and
// manifest.json.
const MaxBatchArchiveSize = 1024*1024*1024 + 64*1024*1024

// DefaultUploadConcurrency is how many batch files upload at once by default.
const DefaultUploadConcurrency = 4

// DefaultBatchTimeout is how long [Client.WaitForBatch], [Client.BatchResults]
// and [Client.DownloadBatchArchive] wait when given no timeout.
const DefaultBatchTimeout = time.Hour

// defaultBatchPoll is the wait between polls when the API sends no Retry-After.
const defaultBatchPoll = 2 * time.Second

var batchID = regexp.MustCompile(`^bat_[a-f0-9]{32}$`)

// BatchItem is one file of a batch with its own forensic data. Set either File
// (the bytes) or Path (a file to read when it is uploaded).
type BatchItem struct {
	// Filename is the file's name; its extension sets the media type. It
	// defaults to the base name of Path.
	Filename string
	// Data is the non-empty forensic JSON for this file (up to 8 KB as JSON).
	Data map[string]any
	// File is the file's content.
	File []byte
	// Path is a file to upload instead of File; it is streamed, not held in memory.
	Path string
}

// BatchOptions are options for [Client.SubmitBatch] and [Client.SubmitBatchZip].
type BatchOptions struct {
	// Archive also zips every result into one download ([Client.DownloadBatchArchive]).
	Archive bool
	// WebhookID is an enabled webhook endpoint (wh_…) that receives one
	// watermark.batch.* event when the batch ends.
	WebhookID string
	// Accelerator requests CPU or GPU processing for every file.
	Accelerator Accelerator
	// StorageDestinationID delivers every result to a verified customer storage
	// destination (dst_…). It cannot be combined with Archive.
	StorageDestinationID string
	// IdempotencyKey makes the submission safely repeatable: 8–128 letters,
	// digits, hyphens or underscores. One is generated when empty; persist your
	// own to resume a batch across process restarts.
	IdempotencyKey string
	// UploadConcurrency is how many files upload at once (default 4).
	UploadConcurrency int
}

// BatchZipItem names one member of a zip sent with [Client.SubmitBatchZip].
type BatchZipItem struct {
	// Filename is the member's path in the zip, exactly as stored.
	Filename string `json:"filename"`
	// Data is the non-empty forensic JSON for this file.
	Data map[string]any `json:"data"`
}

// BatchCounts counts a batch's files by state.
type BatchCounts struct {
	Pending    int `json:"pending"`
	Accepted   int `json:"accepted"`
	Rejected   int `json:"rejected"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	InProgress int `json:"in_progress"`
}

// BatchCredits totals a batch's credits. Failed and rejected files are refunded.
type BatchCredits struct {
	Reserved int `json:"reserved"`
	Charged  int `json:"charged"`
	Refunded int `json:"refunded"`
}

// BatchUpload is the signed URL a pending batch file is uploaded to.
type BatchUpload struct {
	Method    string `json:"method"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// BatchItemStatus is one file of a batch and its state.
type BatchItemStatus struct {
	Index    int     `json:"index"`
	Filename string  `json:"filename"`
	Size     *int    `json:"size"`
	UploadID *string `json:"upload_id"`
	// UploadReceived is true when the file already arrived; such items have no
	// Upload when a submission is replayed.
	UploadReceived bool `json:"upload_received"`
	// RequestID is the file's watermarking job (req_…) once it is accepted.
	RequestID *string `json:"request_id"`
	// Status is "pending", "rejected", "queued", "running", "retrying",
	// "succeeded" or "failed".
	Status string `json:"status"`
	// ErrorCode explains a rejected or failed file, such as
	// "upload_not_received", "upload_size_mismatch", "invalid_input",
	// "insufficient_credits" or "cancelled".
	ErrorCode   *string `json:"error_code"`
	ErrorDetail *string `json:"error_detail"`
	// Credits are the file's reserved or charged credits; 0 when it failed (refunded).
	Credits         *int         `json:"credits"`
	StatusURL       *string      `json:"status_url"`
	ResultURL       *string      `json:"result_url"`
	ResultExpiresAt *string      `json:"result_expires_at"`
	Upload          *BatchUpload `json:"upload"`
}

// Batch is a batch of up to 100 files and the state of each.
type Batch struct {
	BatchID string `json:"batch_id"`
	// Status is "draft", "starting", "processing", "assembling", or the final
	// "completed" (at least one file succeeded), "failed", "cancelled" or
	// "expired" (a draft never started within 24 hours).
	Status               string       `json:"status"`
	ItemCount            int          `json:"item_count"`
	Archive              bool         `json:"archive"`
	Accelerator          Accelerator  `json:"accelerator"`
	WebhookID            *string      `json:"webhook_id"`
	StorageDestinationID *string      `json:"storage_destination_id"`
	Counts               BatchCounts  `json:"counts"`
	Credits              BatchCredits `json:"credits"`
	CancelRequested      bool         `json:"cancel_requested"`
	CreatedAt            string       `json:"created_at"`
	StartedAt            *string      `json:"started_at"`
	CompletedAt          *string      `json:"completed_at"`
	UploadExpiresAt      string       `json:"upload_expires_at"`
	StatusURL            string       `json:"status_url"`
	// ArchiveStatus, ArchiveURL and ArchiveExpiresAt are set only with Archive.
	ArchiveStatus    *string `json:"archive_status"`
	ArchiveURL       *string `json:"archive_url"`
	ArchiveExpiresAt *string `json:"archive_expires_at"`
	// Items lists every file; it is empty in [Client.ListBatches] pages.
	Items []BatchItemStatus `json:"items"`
}

// Done reports whether the batch reached a final state.
func (b *Batch) Done() bool {
	switch b.Status {
	case "completed", "failed", "cancelled", "expired":
		return true
	}
	return false
}

// BatchPage is one page of batches, newest first. Pass NextCursor to
// [Client.ListBatches] to continue; nil means the last page.
type BatchPage struct {
	Data       []Batch `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// BatchItemResult is one file's outcome from [Client.BatchResults].
type BatchItemResult struct {
	Index     int
	Filename  string
	Status    string
	RequestID string
	// ErrorCode explains a file without a result: the API's error code, or
	// "cancelled" or "expired" for files the batch never ran.
	ErrorCode   string
	ErrorDetail string
	// Credits are the credits charged for the file, or nil when none were reserved.
	Credits *int
	// Result is the watermarked file; it is set only for succeeded files.
	Result *EmbedResult
}

// OK reports whether the file was watermarked and its result downloaded.
func (r BatchItemResult) OK() bool { return r.Result != nil }

// SubmitBatch watermarks up to 100 files, each with its own data, as one
// batch: it creates the batch, uploads every file to its signed URL (several
// at a time, never sending the API key there) and starts it. The returned
// batch is usually "starting"; follow it with [Client.WaitForBatch].
//
// Submitting again with the same opts.IdempotencyKey returns the same batch
// and uploads only the files it has not received, so a failed upload can be
// resumed; a batch that already started is returned as it is, and one that
// expired before it started (24 hours) is an error. Files the API rejects at
// start are never charged.
func (c *Client) SubmitBatch(ctx context.Context, items []BatchItem, opts *BatchOptions) (*Batch, error) {
	if len(items) == 0 || len(items) > MaxBatchItems {
		return nil, fmt.Errorf("etchv: a batch takes 1 to %d files; split larger sets into several batches", MaxBatchItems)
	}
	o, err := batchOptions(opts)
	if err != nil {
		return nil, err
	}
	type source struct {
		size int64
		open func() (io.ReadCloser, error)
	}
	sources := make([]source, len(items))
	manifest := make([]map[string]any, len(items))
	for i, item := range items {
		name := item.Filename
		if name == "" && item.Path != "" {
			name = filepath.Base(item.Path)
		}
		if name == "" || len(name) > 255 {
			return nil, fmt.Errorf("etchv: item %d needs a filename of 1 to 255 characters", i)
		}
		if len(item.Data) == 0 {
			return nil, fmt.Errorf("etchv: item %d (%s): data must be a non-empty JSON object", i, name)
		}
		switch {
		case item.File != nil && item.Path != "":
			return nil, fmt.Errorf("etchv: item %d (%s): set File or Path, not both", i, name)
		case item.Path != "":
			info, statErr := os.Stat(item.Path)
			if statErr != nil {
				return nil, fmt.Errorf("etchv: item %d (%s): %w", i, name, statErr)
			}
			path, size := item.Path, info.Size()
			sources[i] = source{size, func() (io.ReadCloser, error) {
				f, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				if info, err := f.Stat(); err != nil || info.Size() != size {
					f.Close()
					return nil, errSizeChanged
				}
				return f, nil
			}}
		default:
			file := item.File
			sources[i] = source{int64(len(file)), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(file)), nil }}
		}
		if sources[i].size == 0 {
			return nil, fmt.Errorf("etchv: item %d (%s): file is empty", i, name)
		}
		manifest[i] = map[string]any{"filename": name, "size": sources[i].size, "data": item.Data}
	}
	body := batchBody(o)
	body["items"] = manifest
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("etchv: encode batch: %w", err)
	}
	var batch Batch
	if err := c.batchCall(ctx, call{method: "POST", path: "watermarks/batches", body: encoded, contentType: "application/json",
		idempotencyKey: o.IdempotencyKey, retry: true, final503: true}, &batch); err != nil {
		return nil, err
	}
	if !batchID.MatchString(batch.BatchID) {
		return nil, &Error{StatusCode: 201, Detail: "invalid batch response", IdempotencyKey: o.IdempotencyKey}
	}
	switch batch.Status {
	case "draft":
	case "expired":
		return nil, &Error{StatusCode: 410, Code: "batch_expired", Detail: fmt.Sprintf("batch %s expired before it was started (a batch must start within 24 hours); submit again with a new IdempotencyKey",
			batch.BatchID), IdempotencyKey: o.IdempotencyKey}
	default:
		return &batch, nil // a replay of a batch that already started
	}

	for _, entry := range batch.Items {
		if entry.Upload == nil {
			if !entry.UploadReceived {
				return nil, &Error{StatusCode: 201, Detail: fmt.Sprintf("invalid batch upload response: item %d (%s) has no upload URL and was not received",
					entry.Index, entry.Filename), IdempotencyKey: o.IdempotencyKey}
			}
			continue
		}
		if entry.Index < 0 || entry.Index >= len(sources) || entry.Upload.Method != "PUT" || !strings.HasPrefix(entry.Upload.URL, "https://") {
			return nil, &Error{StatusCode: 201, Detail: "invalid batch upload response", IdempotencyKey: o.IdempotencyKey}
		}
	}
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		slots    = make(chan struct{}, o.UploadConcurrency)
	)
	for _, entry := range batch.Items {
		if entry.Upload == nil {
			continue // already received (checked above)
		}
		select {
		case slots <- struct{}{}:
		case <-uploadCtx.Done():
		}
		if uploadCtx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(entry BatchItemStatus) {
			defer wg.Done()
			defer func() { <-slots }()
			src := sources[entry.Index]
			if err := c.putSigned(uploadCtx, entry.Upload.URL, src.size, src.open); err != nil {
				once.Do(func() {
					firstErr = uploadError(err, &batch, entry, o.IdempotencyKey)
					cancel()
				})
			}
		}(entry)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if ctx.Err() != nil {
		return nil, &Error{Detail: "context canceled during batch uploads", IdempotencyKey: o.IdempotencyKey, err: ctx.Err()}
	}
	var started Batch
	if err := c.batchCall(ctx, call{method: "POST", path: "watermarks/batches/" + batch.BatchID + "/start", retry: true}, &started); err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.IdempotencyKey == "" {
			apiErr.IdempotencyKey = o.IdempotencyKey
		}
		return nil, err
	}
	return &started, nil
}

func uploadError(err error, batch *Batch, entry BatchItemStatus, key string) error {
	detail := fmt.Sprintf("uploading item %d (%s) of batch %s failed; submit again with the same IdempotencyKey to resume",
		entry.Index, entry.Filename, batch.BatchID)
	e := &Error{Detail: detail, IdempotencyKey: key, err: err}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		e.StatusCode = apiErr.StatusCode
	}
	return e
}

// SubmitBatchZip creates and starts a batch from one zip of up to 55 MB whose
// members are the files; items names every member with its data. Use it for
// files that are already together; [Client.SubmitBatch] suits everything else.
func (c *Client) SubmitBatchZip(ctx context.Context, zip []byte, items []BatchZipItem, opts *BatchOptions) (*Batch, error) {
	if len(items) == 0 || len(items) > MaxBatchItems {
		return nil, fmt.Errorf("etchv: a batch takes 1 to %d files; split larger sets into several batches", MaxBatchItems)
	}
	if len(zip) == 0 || len(zip) > MaxBatchZipSize {
		return nil, fmt.Errorf("etchv: zip must contain 1 byte to %d MB", MaxBatchZipSize/(1024*1024))
	}
	for i, item := range items {
		if item.Filename == "" || len(item.Filename) > 255 {
			return nil, fmt.Errorf("etchv: item %d needs a filename of 1 to 255 characters", i)
		}
		if len(item.Data) == 0 {
			return nil, fmt.Errorf("etchv: item %d (%s): data must be a non-empty JSON object", i, item.Filename)
		}
	}
	o, err := batchOptions(opts)
	if err != nil {
		return nil, err
	}
	manifest := batchBody(o)
	manifest["items"] = items
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("etchv: encode manifest: %w", err)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="archive"; filename="batch.zip"`)
	header.Set("Content-Type", "application/zip")
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err = part.Write(zip); err != nil {
		return nil, err
	}
	if err = w.WriteField("manifest", string(encoded)); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	var batch Batch
	if err := c.batchCall(ctx, call{method: "POST", path: "watermarks/batches/zip", body: body.Bytes(), contentType: w.FormDataContentType(),
		idempotencyKey: o.IdempotencyKey, retry: true, final503: true}, &batch); err != nil {
		return nil, err
	}
	return &batch, nil
}

// GetBatch returns a batch and the state of each of its files.
func (c *Client) GetBatch(ctx context.Context, id string) (*Batch, error) {
	batch, _, err := c.getBatch(ctx, id)
	return batch, err
}

func (c *Client) getBatch(ctx context.Context, id string) (*Batch, http.Header, error) {
	if !batchID.MatchString(id) {
		return nil, nil, errors.New("etchv: invalid batch ID")
	}
	res, err := c.do(ctx, call{method: "GET", path: "watermarks/batches/" + id, retry: true})
	if err != nil {
		return nil, nil, err
	}
	var batch Batch
	if json.Unmarshal(res.body, &batch) != nil {
		return nil, nil, &Error{StatusCode: res.status, Detail: "invalid JSON response", RequestID: res.header.Get("X-Request-ID")}
	}
	return &batch, res.header, nil
}

// WaitForBatch polls a batch, one request at a time as often as the API's
// Retry-After allows, until it reaches a final state, and returns it. A
// timeout of zero waits up to [DefaultBatchTimeout] (one hour). When the
// timeout passes first it returns an [*Error] that wraps
// context.DeadlineExceeded; the batch keeps running.
func (c *Client) WaitForBatch(ctx context.Context, id string, timeout time.Duration) (*Batch, error) {
	if timeout <= 0 {
		timeout = DefaultBatchTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		batch, header, err := c.getBatch(ctx, id)
		if err != nil {
			return nil, err
		}
		if batch.Done() {
			return batch, nil
		}
		delay := batchDelay(header)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &Error{Detail: fmt.Sprintf("batch %s is still %s after %s", id, batch.Status, timeout), err: context.DeadlineExceeded}
		}
		if err := sleep(ctx, min(delay, remaining)); err != nil {
			return nil, &Error{Detail: "context canceled while waiting for the batch", err: err}
		}
	}
}

// BatchResults waits for a batch to finish (as [Client.WaitForBatch], with the
// same timeout rules) and then yields each file's outcome in order,
// downloading succeeded files' results as the loop reaches them (results are
// kept 24 hours). Files that were rejected or failed (and refunded) carry
// their ErrorCode and no Result. A failed download yields that file with the
// error; the loop may continue with the next file. If the wait fails, the
// only value yielded is the error.
//
//	for item, err := range client.BatchResults(ctx, batch.BatchID, 30*time.Minute) {
//		if err != nil { ... }
//		if item.OK() { os.WriteFile(item.Filename, item.Result.Bytes, 0o600) }
//	}
func (c *Client) BatchResults(ctx context.Context, id string, timeout time.Duration) iter.Seq2[BatchItemResult, error] {
	return func(yield func(BatchItemResult, error) bool) {
		batch, err := c.WaitForBatch(ctx, id, timeout)
		if err != nil {
			yield(BatchItemResult{}, err)
			return
		}
		for _, entry := range batch.Items {
			item := BatchItemResult{Index: entry.Index, Filename: entry.Filename, Status: entry.Status, Credits: entry.Credits}
			if entry.RequestID != nil {
				item.RequestID = *entry.RequestID
			}
			if entry.ErrorCode != nil {
				item.ErrorCode = *entry.ErrorCode
			}
			if item.ErrorCode == "" && entry.Status != "succeeded" {
				item.ErrorCode = entry.Status
				if batch.Status == "cancelled" || batch.Status == "expired" {
					item.ErrorCode = batch.Status
				}
			}
			if entry.ErrorDetail != nil {
				item.ErrorDetail = *entry.ErrorDetail
			}
			var downloadErr error
			if entry.Status == "succeeded" && item.RequestID != "" {
				item.Result, downloadErr = c.GetEmbedResult(ctx, item.RequestID)
			}
			if !yield(item, downloadErr) {
				return
			}
		}
	}
}

// DownloadBatchArchive returns the zip of a batch created with Archive: every
// successful result plus manifest.json. See [Client.DownloadBatchArchiveTo],
// which streams it to a writer instead of holding it in memory.
func (c *Client) DownloadBatchArchive(ctx context.Context, id string, timeout time.Duration) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := c.DownloadBatchArchiveTo(ctx, id, &buf, timeout); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DownloadBatchArchiveTo writes the zip of a batch created with Archive to w
// and returns its size. While the batch is still running (HTTP 202) it polls,
// honoring Retry-After, for up to timeout (zero waits up to
// [DefaultBatchTimeout], one hour). The download itself fails only when no
// data arrives for the client timeout, however long the whole zip takes.
// Nothing is written to w unless the archive is ready.
//
// It returns an [*Error] with StatusCode 409 and Code "batch_not_started",
// "archive_not_requested", "archive_too_large" or "archive_unavailable" when
// there is no archive, and 410 when the archive (or a draft) expired.
func (c *Client) DownloadBatchArchiveTo(ctx context.Context, id string, w io.Writer, timeout time.Duration) (int64, error) {
	if !batchID.MatchString(id) {
		return 0, errors.New("etchv: invalid batch ID")
	}
	if timeout <= 0 {
		timeout = DefaultBatchTimeout
	}
	deadline := time.Now().Add(timeout)
	target := c.baseURL + "/watermarks/batches/" + id + "/archive"
	for {
		n, done, delay, err := c.archiveAttempt(ctx, target, w)
		if done {
			return n, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			detail := fmt.Sprintf("the archive of batch %s is not ready yet after %s", id, timeout)
			if err != nil {
				detail = fmt.Sprintf("the archive of batch %s could not be downloaded within %s: %v", id, timeout, err)
			}
			return 0, &Error{StatusCode: 202, Detail: detail, err: context.DeadlineExceeded}
		}
		if sleepErr := sleep(ctx, min(delay, remaining)); sleepErr != nil {
			return 0, &Error{Detail: "context canceled while waiting for the archive", err: sleepErr}
		}
	}
}

// archiveAttempt requests the archive once. done is false when the caller
// should wait delay and ask again (HTTP 202, a retryable status or a transport
// failure before any byte of the zip was written).
func (c *Client) archiveAttempt(ctx context.Context, target string, w io.Writer) (n int64, done bool, delay time.Duration, err error) {
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(c.timeout, cancel)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(attempt, "GET", target, nil)
	if err != nil {
		return 0, true, 0, &Error{Detail: "invalid request", err: err}
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("User-Agent", "etchv-go/"+Version)
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, true, 0, &Error{Detail: "context canceled during the archive download", err: ctx.Err()}
		}
		return 0, false, time.Second, err
	}
	defer res.Body.Close()
	requestID := res.Header.Get("X-Request-ID")
	if res.StatusCode != 200 {
		b, readErr := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		switch {
		case readErr != nil && ctx.Err() != nil:
			return 0, true, 0, &Error{Detail: "context canceled during the archive download", err: ctx.Err()}
		case res.StatusCode == 202:
			return 0, false, batchDelay(res.Header), nil
		case res.StatusCode == 429 || res.StatusCode == 502 || res.StatusCode == 503 || res.StatusCode == 504:
			return 0, false, batchDelay(res.Header), apiError(res.StatusCode, b, res.Header, requestID, "")
		}
		return 0, true, 0, apiError(res.StatusCode, b, res.Header, requestID, "")
	}
	if res.ContentLength > MaxBatchArchiveSize {
		return 0, true, 0, &Error{StatusCode: 200, Code: "archive_too_large", Detail: fmt.Sprintf("archive exceeds %d MB", MaxBatchArchiveSize/(1024*1024)), RequestID: requestID}
	}
	body := &idleReader{r: res.Body, idle: idle, timeout: c.timeout, want: -1}
	// Time spent in the caller's writer is not idle time.
	sink := &pausingWriter{w: w, idle: idle, timeout: c.timeout}
	stalled := func(cause error) error {
		if ctx.Err() != nil {
			return &Error{StatusCode: 200, Detail: "context canceled during the archive download", RequestID: requestID, err: ctx.Err()}
		}
		if attempt.Err() != nil {
			return &Error{StatusCode: 200, Detail: fmt.Sprintf("the archive download stalled: no data for %s", c.timeout), RequestID: requestID, err: context.DeadlineExceeded}
		}
		if sink.err != nil {
			return &Error{StatusCode: 200, Detail: "writing the archive failed", RequestID: requestID, err: sink.err}
		}
		return &Error{StatusCode: 200, Detail: "reading the archive failed", RequestID: requestID, err: cause}
	}
	head := make([]byte, 2)
	if _, err := io.ReadFull(body, head); err != nil || string(head) != "PK" {
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return 0, true, 0, stalled(err)
		}
		return 0, true, 0, &Error{StatusCode: 200, Detail: "invalid archive response", RequestID: requestID}
	}
	if _, err := sink.Write(head); err != nil {
		return 0, true, 0, stalled(err)
	}
	copied, err := io.Copy(sink, io.LimitReader(body, MaxBatchArchiveSize-int64(len(head))+1))
	n = copied + int64(len(head))
	if err != nil {
		return n, true, 0, stalled(err)
	}
	if n > MaxBatchArchiveSize {
		return n, true, 0, &Error{StatusCode: 200, Code: "archive_too_large", Detail: fmt.Sprintf("archive exceeds %d MB", MaxBatchArchiveSize/(1024*1024)), RequestID: requestID}
	}
	return n, true, 0, nil
}

// CancelBatch stops a batch: files not yet running fail as "cancelled" and are
// refunded; files already running finish. A draft is canceled at once.
func (c *Client) CancelBatch(ctx context.Context, id string) (*Batch, error) {
	if !batchID.MatchString(id) {
		return nil, errors.New("etchv: invalid batch ID")
	}
	var batch Batch
	if err := c.batchCall(ctx, call{method: "POST", path: "watermarks/batches/" + id + "/cancel", retry: true}, &batch); err != nil {
		return nil, err
	}
	return &batch, nil
}

// ListBatches returns up to limit batches (1–50; zero uses the API default of
// 20), newest first, without their items. Pass an empty before for the first
// page and the previous page's NextCursor to continue.
func (c *Client) ListBatches(ctx context.Context, limit int, before string) (*BatchPage, error) {
	q := url.Values{}
	if limit < 0 || limit > 50 {
		return nil, errors.New("etchv: limit must be between 1 and 50")
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before != "" {
		if !batchID.MatchString(before) {
			return nil, errors.New("etchv: invalid batch cursor")
		}
		q.Set("before", before)
	}
	var page BatchPage
	if err := c.json(ctx, "GET", "watermarks/batches", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func batchOptions(opts *BatchOptions) (BatchOptions, error) {
	var o BatchOptions
	if opts != nil {
		o = *opts
	}
	if o.WebhookID != "" && !webhookID.MatchString(o.WebhookID) {
		return o, errors.New("etchv: invalid webhook ID")
	}
	if o.StorageDestinationID != "" && !destinationID.MatchString(o.StorageDestinationID) {
		return o, errors.New("etchv: invalid storage destination ID")
	}
	if o.Accelerator != "" && !o.Accelerator.valid() {
		return o, errors.New(`etchv: accelerator must be "cpu" or "gpu"`)
	}
	if o.UploadConcurrency < 0 {
		return o, errors.New("etchv: upload concurrency must be at least 1")
	}
	if o.UploadConcurrency == 0 {
		o.UploadConcurrency = DefaultUploadConcurrency
	}
	if o.IdempotencyKey == "" {
		var key [16]byte
		if _, err := rand.Read(key[:]); err != nil {
			return o, err
		}
		o.IdempotencyKey = hex.EncodeToString(key[:])
	} else if !idempotencyKey.MatchString(o.IdempotencyKey) {
		return o, errors.New("etchv: idempotency key must contain 8–128 letters, digits, hyphens or underscores")
	}
	return o, nil
}

func batchBody(o BatchOptions) map[string]any {
	body := map[string]any{"archive": o.Archive}
	if o.WebhookID != "" {
		body["webhook_id"] = o.WebhookID
	}
	if o.Accelerator != "" {
		body["accelerator"] = o.Accelerator
	}
	if o.StorageDestinationID != "" {
		body["storage_destination_id"] = o.StorageDestinationID
	}
	return body
}

// batchCall sends r and decodes a batch view.
func (c *Client) batchCall(ctx context.Context, r call, batch *Batch) error {
	res, err := c.do(ctx, r)
	if err != nil {
		return err
	}
	if json.Unmarshal(res.body, batch) != nil {
		return &Error{StatusCode: res.status, Detail: "invalid JSON response", RequestID: res.header.Get("X-Request-ID"), IdempotencyKey: r.idempotencyKey}
	}
	return nil
}

// batchDelay is the API's Retry-After (at least one second), or two seconds
// when it sent none.
func batchDelay(h http.Header) time.Duration {
	if d := retryAfter(h.Get("Retry-After"), time.Now()); d > 0 {
		return max(d, time.Second)
	}
	return defaultBatchPoll
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pausingWriter stops an idle timer while the caller's writer runs.
type pausingWriter struct {
	w       io.Writer
	idle    *time.Timer
	timeout time.Duration
	err     error
}

func (p *pausingWriter) Write(b []byte) (int, error) {
	p.idle.Stop()
	defer p.idle.Reset(p.timeout)
	n, err := p.w.Write(b)
	if err != nil {
		p.err = err
	}
	return n, err
}
