// Package database — downloader.go.
//
// S3-compatible streaming downloader used by restore_dump. The inverse
// of uploader.go: we expose an io.ReadCloser over a single S3 object
// (the encrypted ciphertext written by a prior export_dump) so the
// decrypter downstream can consume it without ever materializing the
// blob on local disk.
//
// Internally:
//
//   - One HEAD request on construction to learn the object size, which is
//     surfaced as Total so the orchestrator can emit a determinate
//     bytes_total_estimate to the frontend.
//   - One GetObject request to open a streaming response body. The body
//     is read on demand by the caller; the AWS SDK handles HTTP framing
//     and chunked transfer encoding.
//   - Progress callbacks fire at most once per progressTickInterval
//     (coalesced via a uint64 cumulative counter — the orchestrator
//     samples the counter on its own ticker, so we only update the
//     atomic per Read, never invoke the callback inside Read itself).
//
// The same code path serves AWS S3, MinIO, Cloudflare R2, and any other
// S3-API-compatible store. Path-style addressing and the endpoint
// override are honored identically to the uploader.
//
// We deliberately bypass the SDK's `feature/s3/manager.Downloader`. That
// helper buffers the entire object to a Writer in concurrent ranges and
// is optimized for parallel throughput; here we want pure streaming
// semantics with strict order preservation (the decrypter is not
// random-access).
package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ChunkDownloader is an io.ReadCloser that streams an S3 object's body
// straight through to the caller. NOT safe for concurrent use; one
// reader per download.
type ChunkDownloader struct {
	// Total is the object's content length as reported by S3 HEAD. It is
	// populated before NewChunkDownloader returns; the orchestrator uses
	// it as the bytes_total_estimate for progress reporting.
	Total uint64

	body io.ReadCloser
	read atomic.Uint64 // cumulative bytes successfully delivered to caller
}

// NewChunkDownloader issues HEAD + GetObject and returns a reader ready
// to stream. The caller MUST invoke Close() to release the underlying
// HTTP response body, even on error mid-stream — otherwise the
// connection is leaked back to the SDK transport pool in a bad state.
//
// HEAD-then-GET is two round trips, but it gives us the total size up
// front (S3's GetObject returns ContentLength too, but it is informally
// reported and some compatibility layers — older MinIO, R2 — populate it
// inconsistently for ranged reads). HEAD is the canonical sizing call.
func NewChunkDownloader(
	ctx context.Context,
	dest *DestinationDescriptor,
	objectKey string,
) (*ChunkDownloader, error) {
	if dest == nil {
		return nil, errors.New("downloader: nil destination")
	}
	if dest.Bucket == "" {
		return nil, errors.New("downloader: empty bucket")
	}
	if objectKey == "" {
		return nil, errors.New("downloader: empty object key")
	}
	if dest.Region == "" {
		return nil, errors.New("downloader: empty region")
	}
	if dest.AccessKeyID == "" || dest.SecretAccessKey == "" {
		return nil, errors.New("downloader: missing access credentials")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(dest.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(dest.AccessKeyID, dest.SecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("downloader: load aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		// Mirror the uploader's MinIO/R2/on-prem handling exactly so a
		// bucket that exports cleanly always restores cleanly.
		if dest.UsePathStyle {
			o.UsePathStyle = true
		}
		if dest.Endpoint != "" {
			endpoint := dest.Endpoint
			o.BaseEndpoint = &endpoint
		}
	})

	// HEAD first: gives us the total size and surfaces auth/permission
	// failures before we open a long-lived streaming body. This call is
	// cheap and fail-fast.
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(dest.Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("downloader: head object: %w", err)
	}

	var total uint64
	if head.ContentLength != nil && *head.ContentLength >= 0 {
		total = uint64(*head.ContentLength)
	}

	getOut, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(dest.Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("downloader: get object: %w", err)
	}

	d := &ChunkDownloader{
		Total: total,
		body:  getOut.Body,
	}
	return d, nil
}

// Read pulls bytes from the underlying S3 response body and updates the
// cumulative byte counter. The orchestrator samples Read() (via
// Downloaded()) on its own ticker so we never block Read on a callback.
func (d *ChunkDownloader) Read(p []byte) (int, error) {
	n, err := d.body.Read(p)
	if n > 0 {
		d.read.Add(uint64(n))
	}
	return n, err
}

// Downloaded returns the cumulative number of bytes successfully
// delivered to the caller through Read. Safe to call concurrently with
// Read.
func (d *ChunkDownloader) Downloaded() uint64 {
	return d.read.Load()
}

// TotalBytes returns the object's content length as reported by S3
// HEAD. Implements the readerSource contract so the restore pipeline
// can swap between ChunkDownloader and LocalFileSource without
// per-source branching at the progress-reporting layer.
func (d *ChunkDownloader) TotalBytes() uint64 {
	return d.Total
}

// Close releases the underlying HTTP response body. Idempotent: a second
// Close is a no-op. Always call this from a defer in the orchestrator,
// regardless of whether the read completed successfully.
func (d *ChunkDownloader) Close() error {
	if d.body == nil {
		return nil
	}
	body := d.body
	d.body = nil
	if err := body.Close(); err != nil {
		return fmt.Errorf("downloader: close body: %w", err)
	}
	return nil
}
