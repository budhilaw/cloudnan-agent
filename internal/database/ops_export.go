// Package database — ops_export.go.
//
// export_dump orchestrator. Wires the dump tool's stdout into a
// streaming AES-256-GCM encrypter into an S3 multipart uploader, all
// without ever materializing the plaintext on disk.
//
// Pipeline (left-to-right is the data flow):
//
//	mysqldump|pg_dump --stdout-->  [optional gzip]  -->  AES-GCM stream
//	  -->  S3 multipart upload
//
// The pipeline is built top-down: we open the uploader first (it issues
// CreateMultipartUpload, which fails fast on bad credentials), then wrap
// it in the encrypter, then optionally in gzip, then point the dump
// tool's stdout at the head. A 1 MiB io.Copy buffer keeps throughput up.
//
// Progress is reported via NDJSON chunks of pb.DatabaseExportProgress
// emitted through the agent's emit() callback. The emitter ticks once
// per second to publish the running uploaded-byte total, and emits
// terminal DUMPING / UPLOADING / COMPLETED / FAILED chunks at phase
// transitions.
//
// Security guarantees enforced here:
//
//   - The 32-byte encryption key is zeroed before opExportDump returns.
//     The caller (the agent transport) keeps no reference to the key
//     after handing the request envelope to us.
//   - The uploader is unconditionally aborted on any error path so
//     orphan parts never linger on the destination.
//   - Context cancellation kills the dump subprocess (CommandContext
//     does this) and aborts the upload (the deferred Abort runs).
//   - Temp files materialized for TLS CAs are content-addressed and
//     reused across calls; no per-export cleanup needed.
package database

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/cloudnan-tech/cloudnan-agent/proto/agent"
)

// localExportRoot is the on-agent directory where Cloudnan-Storage
// destination dumps land before the control plane fetches them. Kept
// under /tmp so it's wiped on host reboot — a stale ciphertext lying
// around from a crashed agent cleans itself up rather than persisting
// and consuming disk forever. The control plane is responsible for
// removing per-export files immediately after a successful fetch (the
// runID-suffixed object key gives every export its own subtree, so a
// race between two exports can't collide).
const localExportRoot = "/tmp/cloudnan-dbexport"

// ExportDumpEnvelope is the agent-side payload for export_dump. Wraps the
// proto request plus pre-resolved destination credentials. The control
// plane JSON-marshals this and ships it as args[1]. Proto is reserved
// for the stable cross-language wire; this envelope holds derived
// runtime values (the resolved destination credentials and the
// confirmation token, neither of which is in the proto today).
type ExportDumpEnvelope struct {
	// Request is the protojson-encoded pb.DatabaseExportDumpRequest.
	// We carry it as RawMessage so we can re-decode with protojson and
	// pick up any proto3 defaulting.
	Request json.RawMessage `json:"request"`

	// Destination is the pre-resolved S3-compatible target. The control
	// plane looks up backup_destinations.destination_id from the proto
	// request and fills in real credentials here. The agent never reaches
	// back to the control plane to get this — by the time export_dump
	// runs, everything it needs is in the envelope.
	Destination DestinationDescriptor `json:"destination"`

	// ConfirmationToken is the HMAC-SHA256 token minted by the control
	// plane that pins this export to a specific (op, instance, target).
	// Verified against verifyOpToken below. Carried in the envelope
	// because the existing proto request has no field for it.
	ConfirmationToken string `json:"confirmation_token"`
}

// DestinationDescriptor is the resolved S3-compatible destination for an
// export. All fields are populated by the control plane from the
// backup_destinations table.
type DestinationDescriptor struct {
	Provider        string `json:"provider"`           // "s3" | "minio" | "r2" | "gcs"
	Endpoint        string `json:"endpoint,omitempty"` // empty for AWS, set for MinIO/R2
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	PathPrefix      string `json:"path_prefix"`        // includes trailing /
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	UsePathStyle    bool   `json:"use_path_style"`     // true for MinIO
}

const (
	// progressTickInterval is how often we publish a progress chunk
	// during the UPLOADING phase. 1 s is responsive enough for a UI
	// progress bar without flooding the gRPC stream.
	progressTickInterval = 1 * time.Second

	// abortTimeout is the deadline for the AbortMultipartUpload RPC
	// issued during cleanup. Kept short — if S3 is unreachable we have
	// bigger problems than a stranded multipart.
	abortTimeout = 30 * time.Second

	// copyBufferSize is the byte buffer io.CopyBuffer uses inside the
	// pipeline. 1 MiB matches the encrypter block boundary so a single
	// Write into the encrypter triggers exactly one Seal/UploadPart pair
	// at the high-water mark.
	copyBufferSize = 1 << 20
)

// opExportDump is the agent-side export_dump implementation. Returns
// nil on success; on failure returns an error describing what went
// wrong AFTER having emitted a PHASE_FAILED progress chunk so the
// frontend has a structured view of the failure.
func (h *Handler) opExportDump(ctx context.Context, args []string, emit func(string)) error {
	if len(args) < 2 {
		return errors.New("missing JSON envelope in args[1]")
	}

	// ---- 1. parse envelope + proto request ----
	var env ExportDumpEnvelope
	if err := json.Unmarshal([]byte(args[1]), &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	req := &pb.DatabaseExportDumpRequest{}
	if err := protojson.Unmarshal(env.Request, req); err != nil {
		return fmt.Errorf("decode export request: %w", err)
	}

	// Always scrub the encryption key bytes before returning, no matter
	// which exit path we take. We hold it in req.EncryptionKey only as
	// long as the encrypter needs it (the encrypter keeps its own derived
	// AEAD instance, not the raw key).
	defer scrubBytes(req.EncryptionKey)

	// ---- 2. validate ----
	if req.GetInstance().GetInstanceId() == "" {
		return emitFailureAndError(emit, "instance.instance_id is required")
	}
	if len(req.GetEncryptionKey()) != keySize {
		return emitFailureAndError(emit, fmt.Sprintf("encryption_key must be %d bytes (AES-256)", keySize))
	}
	if req.GetDestination() == nil {
		return emitFailureAndError(emit, "destination is required")
	}

	// ---- 3. confirmation-token gate ----
	// The token's "target" is the comma-joined list of database names so
	// the control plane signs exactly the export the user clicked on.
	// Empty database list (= all databases) signs as the empty string.
	target := strings.Join(req.GetDatabaseNames(), ",")
	if err := verifyOpToken(
		env.ConfirmationToken,
		"export_dump",
		req.GetInstance().GetInstanceId(),
		target,
	); err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("confirmation token: %v", err))
	}

	// ---- 4. resolve credentials + dumper ----
	vault, err := h.ensureVault()
	if err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("vault: %v", err))
	}
	cred, err := vault.Get(req.GetInstance().GetInstanceId())
	if err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("vault get %s: %v", req.GetInstance().GetInstanceId(), err))
	}
	engineEnum, ok := engineStringToEnum(cred.Engine)
	if !ok {
		return emitFailureAndError(emit, fmt.Sprintf("unknown engine %q in vault", cred.Engine))
	}
	dumper, err := dumperFor(engineEnum)
	if err != nil {
		return emitFailureAndError(emit, err.Error())
	}

	// ---- 5. build object key ----
	objectKey, err := buildObjectKey(env.Destination.PathPrefix, cred.Engine, req.GetInstance().GetInstanceId(), req.GetCompress())
	if err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("build object key: %v", err))
	}

	currentDB := ""
	if len(req.GetDatabaseNames()) > 0 {
		currentDB = req.GetDatabaseNames()[0]
	}

	// ---- 6. emit DUMPING phase ----
	emitProgress(emit, &pb.DatabaseExportProgress{
		Phase:           pb.DatabaseExportPhase_DATABASE_EXPORT_PHASE_DUMPING,
		CurrentDatabase: currentDB,
		ObjectKey:       objectKey,
	})

	// ---- 7. estimate total size (best-effort) ----
	// We open a *separate* short-lived pool to ask the server how large
	// the export will be. Failure here is non-fatal — we just emit
	// bytes_total_estimate=0 and the frontend renders an indeterminate
	// progress bar. This MUST happen before we spawn the dump tool so
	// the dump tool's snapshot doesn't include our metadata query.
	totalEstimate := estimateDumpSize(ctx, h, req.GetInstance().GetInstanceId(), engineEnum, req.GetDatabaseNames())

	// ---- 8. open sink (fails fast on bad S3 creds, or unwritable local
	// path for cloudnan) ----
	//
	// Branching point between S3-style destinations and Cloudnan
	// Storage. The pipeline below (gzip → encrypter → sink) is identical
	// either way; only the byte-destination differs. ChunkUploader
	// streams to S3 multipart; LocalFileSink writes to /tmp on the agent
	// host and the control plane fetches it post-COMPLETED. See
	// internal/database/sink.go for the rationale.
	var (
		sink           uploadSink
		uploadedAtomic atomic.Uint64
	)
	if env.Destination.Provider == "cloudnan" {
		// Local-file sink: control plane post-fetches from this path.
		// It also forms the COMPLETED object_key the orchestrator uses
		// to locate the file.
		localPath := filepath.Join(localExportRoot, objectKey)
		lfs, err := NewLocalFileSink(localPath)
		if err != nil {
			return emitFailureAndError(emit, fmt.Sprintf("local file sink: %v", err))
		}
		sink = lfs
		// LocalFileSink doesn't have an onProgress callback; we sample
		// Uploaded() from the ticker below. Set the atomic from a
		// background goroutine so the existing ticker code is unchanged.
		// One read per progressTickInterval is cheap.
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					uploadedAtomic.Store(lfs.Uploaded())
					time.Sleep(progressTickInterval / 4)
				}
			}
		}()
	} else {
		onProgress := func(n uint64) { uploadedAtomic.Store(n) }
		cu, err := NewChunkUploader(ctx, &env.Destination, objectKey, onProgress)
		if err != nil {
			return emitFailureAndError(emit, fmt.Sprintf("uploader: %v", err))
		}
		sink = cu
	}
	// Defer abort: only fires if Close was never called or returned an
	// error. The successful path calls sink.Close() explicitly and
	// then this defer's Abort() is a no-op (already closed).
	uploadAborted := false
	defer func() {
		if !uploadAborted {
			_ = sink.Abort()
		}
	}()

	// ---- 9. wire encrypter on top of sink ----
	encrypter, err := NewStreamEncrypter(req.GetEncryptionKey(), sink)
	if err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("encrypter: %v", err))
	}

	// ---- 10. optional gzip on top of encrypter ----
	// Order: dump --> gzip --> encrypt --> upload. Compress before encrypt
	// because ciphertext is incompressible. We use BestSpeed because the
	// CPU cost of gzip dominates the dump pipeline at higher levels and
	// the marginal compression beyond -1 is small for SQL text.
	var headOfPipeline io.WriteCloser = encrypter
	var gzipWriter *gzip.Writer
	if req.GetCompress() {
		gw, err := gzip.NewWriterLevel(encrypter, gzip.BestSpeed)
		if err != nil {
			return emitFailureAndError(emit, fmt.Sprintf("gzip: %v", err))
		}
		gzipWriter = gw
		headOfPipeline = gw
	}

	// ---- 11. spawn dump tool ----
	program, dumpArgs, dumpEnv, err := dumper.Args(cred, req.GetDatabaseNames())
	if err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("build dump args: %v", err))
	}

	// CommandContext kills the child if ctx is canceled. This is the
	// agent's only handle on the dump subprocess after Start().
	cmd := exec.CommandContext(ctx, program, dumpArgs...)
	cmd.Env = append(cmd.Environ(), dumpEnv...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("stdout pipe: %v", err))
	}
	// Capture stderr so we can include the tool's diagnostic output in
	// any failure. Bound the buffer because pg_dump --verbose can be
	// chatty and we don't want unbounded memory use on very long dumps.
	stderrCap := newBoundedWriter(64 * 1024)
	cmd.Stderr = stderrCap

	if err := cmd.Start(); err != nil {
		return emitFailureAndError(emit, fmt.Sprintf("start %s: %v", program, err))
	}

	// ---- 12. emit UPLOADING phase + start progress ticker ----
	emitProgress(emit, &pb.DatabaseExportProgress{
		Phase:              pb.DatabaseExportPhase_DATABASE_EXPORT_PHASE_UPLOADING,
		CurrentDatabase:    currentDB,
		ObjectKey:          objectKey,
		BytesTotalEstimate: totalEstimate,
	})

	tickerCtx, cancelTicker := context.WithCancel(ctx)
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		ticker := time.NewTicker(progressTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-tickerCtx.Done():
				return
			case <-ticker.C:
				emitProgress(emit, &pb.DatabaseExportProgress{
					Phase:              pb.DatabaseExportPhase_DATABASE_EXPORT_PHASE_UPLOADING,
					BytesProcessed:     uploadedAtomic.Load(),
					BytesTotalEstimate: totalEstimate,
					CurrentDatabase:    currentDB,
					ObjectKey:          objectKey,
				})
			}
		}
	}()

	// ---- 13. pump bytes ----
	// io.CopyBuffer with a 1-MiB buffer aligns flushes with the encrypter
	// block size — every io.Copy iteration delivers exactly one block to
	// the encrypter, which seals one block, which goes into the uploader.
	copyBuf := make([]byte, copyBufferSize)
	_, copyErr := io.CopyBuffer(headOfPipeline, stdout, copyBuf)

	// Drain the dump subprocess. Wait() finalizes the exit status; we
	// always call it, regardless of copy success, so we don't leak a
	// zombie. If Wait reports a non-zero exit, that's the canonical error.
	waitErr := cmd.Wait()

	// Stop the ticker now that no more bytes will flow through. Wait for
	// the goroutine to exit so we don't race with the final emit below.
	cancelTicker()
	<-tickerDone

	if copyErr != nil {
		return finalizeFailure(emit, sink, &uploadAborted, fmt.Sprintf("pipe copy: %v (dump stderr: %s)", copyErr, stderrCap.String()))
	}
	if waitErr != nil {
		return finalizeFailure(emit, sink, &uploadAborted, fmt.Sprintf("%s exit: %v (stderr: %s)", program, waitErr, stderrCap.String()))
	}

	// ---- 14. close pipeline in order: gzip -> encrypter -> sink ----
	// Each Close flushes its tail; closing them out of order would either
	// drop unflushed bytes or write to a closed downstream sink.
	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			return finalizeFailure(emit, sink, &uploadAborted, fmt.Sprintf("gzip close: %v", err))
		}
	}
	if err := encrypter.Close(); err != nil {
		return finalizeFailure(emit, sink, &uploadAborted, fmt.Sprintf("encrypter close: %v", err))
	}
	if err := sink.Close(); err != nil {
		uploadAborted = true // Close() already aborted internally on error
		return emitFailureAndError(emit, fmt.Sprintf("sink close: %v", err))
	}
	uploadAborted = true // Close() succeeded; defer-Abort is a no-op now.

	// ---- 15. emit COMPLETED ----
	finalSize := sink.Uploaded()
	emitProgress(emit, &pb.DatabaseExportProgress{
		Phase:              pb.DatabaseExportPhase_DATABASE_EXPORT_PHASE_COMPLETED,
		BytesProcessed:     finalSize,
		BytesTotalEstimate: totalEstimate,
		CurrentDatabase:    currentDB,
		ObjectKey:          objectKey,
		FinalSizeBytes:     finalSize,
	})
	return nil
}

// ----- helpers -----

// emitProgress serializes msg as protojson + '\n' (NDJSON) and pushes it.
// Marshal failure is logged as an error chunk; we never silently drop
// progress updates.
func emitProgress(emit func(string), msg *pb.DatabaseExportProgress) {
	b, err := protojson.Marshal(msg)
	if err != nil {
		// Fall back to a hand-built JSON error message; we cannot let a
		// marshal bug brick the entire export.
		emit(fmt.Sprintf(`{"phase":"DATABASE_EXPORT_PHASE_FAILED","error_message":"protojson marshal: %s"}`+"\n", err.Error()))
		return
	}
	emit(string(b) + "\n")
}

// emitFailureAndError emits a PHASE_FAILED chunk and returns an error
// carrying the same message. Used by the validation-failure exit paths
// before the uploader is constructed, so there's nothing to abort.
func emitFailureAndError(emit func(string), message string) error {
	emitProgress(emit, &pb.DatabaseExportProgress{
		Phase:        pb.DatabaseExportPhase_DATABASE_EXPORT_PHASE_FAILED,
		ErrorMessage: message,
	})
	return errors.New(message)
}

// finalizeFailure emits PHASE_FAILED, aborts the in-flight sink (S3
// multipart abort or local-file removal depending on which sink is
// active), marks it aborted so the deferred Abort() is a no-op, and
// returns the error.
func finalizeFailure(emit func(string), sink uploadSink, aborted *bool, message string) error {
	if sink != nil && aborted != nil && !*aborted {
		_ = sink.Abort()
		*aborted = true
	}
	emitProgress(emit, &pb.DatabaseExportProgress{
		Phase:        pb.DatabaseExportPhase_DATABASE_EXPORT_PHASE_FAILED,
		ErrorMessage: message,
	})
	return errors.New(message)
}

// scrubBytes overwrites b with zeros. Used to wipe key material after
// the encrypter no longer needs it. Constant-time semantics aren't
// required (we're not comparing) but the overwrite must actually
// happen — the compiler can't elide writes to the Go heap that escapes
// to a deferred function.
func scrubBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// buildObjectKey assembles the destination object key:
//
//	<prefix>/<engine>-<instance_id>-<UTCYYYYMMDDHHMMSS>-<rand8hex>[.gz].dump.enc
//
// The random suffix prevents two simultaneous exports of the same
// instance from colliding even if they share a timestamp.
func buildObjectKey(prefix, engine, instanceID string, compress bool) (string, error) {
	rnd := make([]byte, 4) // 4 bytes -> 8 hex chars
	if _, err := io.ReadFull(rand.Reader, rnd); err != nil {
		return "", fmt.Errorf("random suffix: %w", err)
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	suffix := ".dump.enc"
	if compress {
		suffix = ".gz" + suffix
	}
	// Ensure the prefix has its trailing slash so the joined path matches
	// what the operator configured in backup_destinations. Empty prefix
	// is allowed (object lands at the bucket root).
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return fmt.Sprintf("%s%s-%s-%s-%s%s",
		prefix,
		safeKeyComponent(engine),
		safeKeyComponent(instanceID),
		ts,
		hex.EncodeToString(rnd),
		suffix,
	), nil
}

// safeKeyComponent strips characters that are problematic in S3 object
// keys (slashes would create unwanted "directories", spaces and control
// chars cause client-side breakage).
func safeKeyComponent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// estimateDumpSize asks the server for an upper-bound size estimate so
// the frontend can render a determinate progress bar. Returns 0 (= "no
// estimate") on any error, including unsupported-engine paths.
func estimateDumpSize(ctx context.Context, h *Handler, instanceID string, engine pb.DatabaseEngine, dbNames []string) uint64 {
	estCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, db, _, err := h.openInstance(estCtx, instanceID)
	if err != nil {
		return 0
	}
	defer func() { _ = db.Close() }()

	switch engine {
	case pb.DatabaseEngine_DATABASE_ENGINE_MYSQL,
		pb.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		// information_schema.TABLES sums data_length+index_length per DB.
		// This is the on-disk footprint, which is a generous upper bound
		// on the dump size (dumps are usually 30–50% smaller because they
		// omit indexes and use efficient INSERT batching).
		var total uint64
		var query string
		var queryArgs []interface{}
		switch len(dbNames) {
		case 0:
			query = "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.TABLES WHERE table_schema NOT IN ('mysql','information_schema','performance_schema','sys')"
		default:
			placeholders := strings.Repeat("?,", len(dbNames))
			placeholders = placeholders[:len(placeholders)-1]
			query = "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.TABLES WHERE table_schema IN (" + placeholders + ")"
			for _, n := range dbNames {
				queryArgs = append(queryArgs, n)
			}
		}
		if err := db.QueryRowContext(estCtx, query, queryArgs...).Scan(&total); err != nil {
			return 0
		}
		return total
	case pb.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		if len(dbNames) != 1 {
			return 0
		}
		// pg_database_size returns bytes including indexes and free space
		// inside the database files. The dump is typically smaller, so
		// this is a safe upper bound for the progress bar.
		var total uint64
		if err := db.QueryRowContext(estCtx, "SELECT pg_database_size($1)", dbNames[0]).Scan(&total); err != nil {
			return 0
		}
		return total
	}
	return 0
}

// boundedWriter is a circular buffer wrapped as an io.Writer. Captures
// the LAST cap bytes written; older content is dropped. Used to retain
// the tail of the dump tool's stderr without an unbounded buffer.
type boundedWriter struct {
	cap int
	buf []byte
}

func newBoundedWriter(cap int) *boundedWriter {
	return &boundedWriter{cap: cap, buf: make([]byte, 0, cap)}
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) >= w.cap {
		w.buf = append(w.buf[:0], p[len(p)-w.cap:]...)
		return len(p), nil
	}
	overflow := len(w.buf) + len(p) - w.cap
	if overflow > 0 {
		// Drop overflow bytes from the head of buf.
		w.buf = w.buf[overflow:]
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *boundedWriter) String() string { return string(w.buf) }
