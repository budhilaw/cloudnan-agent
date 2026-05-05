// Package database — ops_restore.go.
//
// restore_dump orchestrator. Inverse of ops_export.go: pulls the
// encrypted ciphertext from S3, authenticates and decrypts each frame,
// optionally gunzips, and pipes the plaintext into the engine-specific
// restore tool's stdin.
//
// Pipeline (left-to-right is the data flow):
//
//	S3 GetObject  -->  AES-GCM stream decrypt  -->  [optional gunzip]
//	  -->  mysql|pg_restore stdin  -->  target database
//
// The pipeline is built bottom-up: we open the downloader first (HEAD +
// GetObject, which fails fast on bad creds or missing object), then
// wrap it in a decrypter, then optionally a gzip reader, then point the
// restore tool's stdin at the head. A 1 MiB io.Copy buffer keeps
// throughput aligned with the encrypter's block size.
//
// Progress is reported via NDJSON RestoreProgressEvent chunks emitted
// through the agent's emit() callback. The emitter ticks once per second
// to publish the running downloaded-byte total, and emits terminal
// preparing / downloading / restoring / completed / failed chunks at
// phase transitions.
//
// Security guarantees enforced here:
//
//   - The 32-byte encryption key is zeroed before opRestoreDump returns.
//     The orchestrator holds it in env.EncryptionKey only as long as the
//     decrypter's AEAD instance is alive.
//   - The downloader's HTTP body is unconditionally closed on every exit
//     path so connections do not leak.
//   - Context cancellation kills the restore subprocess (CommandContext
//     does this) and races the body close.
//   - The confirmation token is verified before any DB connection or S3
//     RPC fires; an unsigned restore request gets no network footprint.
//   - The target database name is regex-validated AND quoted before any
//     SQL touches the engine. We never trust an inbound name.
//   - The agent refuses to restore on top of an existing database; the
//     control plane is expected to mint a fresh `_restore_<ts>` name.
//   - Temp files for TLS CAs are content-addressed and reused across
//     calls; no per-restore cleanup needed.
package database

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"time"

	pb "github.com/cloudnan-tech/cloudnan-agent/proto/agent"
)

// localRestoreRoot is the on-agent directory where the control plane
// pre-pushes Cloudnan-Storage-sourced ciphertext before dispatching
// the restore command. Mirror of localExportRoot but per-direction so
// an export running concurrently with a restore on the same agent
// can't collide on filenames. /tmp so files self-clean on reboot if
// our explicit cleanup ever fails.
const localRestoreRoot = "/tmp/cloudnan-dbrestore"

// RestoreDumpEnvelope is the agent payload for restore_dump. Mirrors
// ExportDumpEnvelope's structure (proto request + non-proto destination)
// but is fully JSON because the proto has no restore_dump messages yet.
// The control plane JSON-marshals this and ships it as args[1].
type RestoreDumpEnvelope struct {
	// InstanceID is the vault key that resolves to the target instance's
	// admin credentials. Required.
	InstanceID string `json:"instance_id"`

	// Engine is the engine name string ("mysql" / "mariadb" /
	// "postgresql"). Cross-checked against the vault entry; a mismatch
	// fails the restore (the operator should never be asking us to
	// restore a postgres dump into a mysql instance).
	Engine string `json:"engine"`

	// SourceObjectKey is the exact S3 object key that a prior
	// export_dump wrote. Required; the agent does no listing or
	// discovery.
	SourceObjectKey string `json:"source_object_key"`

	// Destination is the resolved S3-compatible source bucket —
	// reused from export so the same bucket that holds the export is
	// where the restore reads from. Required.
	Destination DestinationDescriptor `json:"destination"`

	// EncryptionKey is the 32-byte AES-256 key the export used. The
	// agent scrubs this slice before returning. Required.
	EncryptionKey []byte `json:"encryption_key"`

	// Compressed indicates whether the export pipelined gzip before
	// AES-GCM. Must match the export; getting it wrong yields garbage
	// at the restore tool's stdin and aborts mid-stream.
	Compressed bool `json:"compressed"`

	// TargetDatabase is the freshly-named database the agent creates
	// before the restore. Must match `^[a-zA-Z_][a-zA-Z0-9_]*$` to
	// keep us safely outside any quoting edge cases. Required.
	TargetDatabase string `json:"target_database"`

	// OwnerUsername / OwnerHost optionally identify a role that should
	// own the restored database. Empty leaves ownership at the
	// connecting admin user (Postgres) or implicit (MySQL).
	OwnerUsername string `json:"owner_username,omitempty"`
	OwnerHost     string `json:"owner_host,omitempty"`

	// ConfirmationToken is the HMAC-SHA256 token minted by the control
	// plane that pins this restore to a specific (op, instance, target).
	// Verified against verifyOpToken below.
	ConfirmationToken string `json:"confirmation_token"`
}

// RestoreProgressEvent is the NDJSON chunk shape emitted on the
// orchestrator's emit() callback. We use a parallel struct rather than
// reusing pb.DatabaseExportProgress because the restore phase set is
// semantically different (downloading + decrypting + restoring vs
// dumping + uploading) and overloading the export enum would be
// confusing in logs.
type RestoreProgressEvent struct {
	Phase              string `json:"phase"`
	BytesProcessed     uint64 `json:"bytes_processed"`
	BytesTotalEstimate uint64 `json:"bytes_total_estimate"`
	TargetDatabase     string `json:"target_database"`
	ErrorMessage       string `json:"error_message,omitempty"`
}

// Phase constants for RestoreProgressEvent.Phase. Kept in lower-snake-
// case so frontends can switch on them without protobuf-style enum
// noise.
const (
	restorePhasePending     = "pending"
	restorePhasePreparing   = "preparing"
	restorePhaseDownloading = "downloading"
	restorePhaseRestoring   = "restoring"
	restorePhaseCompleted   = "completed"
	restorePhaseFailed      = "failed"
)

// targetDatabaseRe is the strict whitelist for restore target names.
// We require a leading letter or underscore, followed by alphanumerics
// or underscores. This excludes hyphens (rare in practice; let the
// control plane substitute underscores) and any quoting-sensitive
// characters. The dump's internal CREATE TABLE statements may use any
// identifier — we only constrain the OUTER target name we create here.
var targetDatabaseRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// opRestoreDump is the agent-side restore_dump implementation. Returns
// nil on success; on failure returns an error AFTER having emitted a
// failed RestoreProgressEvent so the frontend has a structured view of
// the failure.
func (h *Handler) opRestoreDump(ctx context.Context, args []string, emit func(string)) error {
	if len(args) < 2 {
		return errors.New("missing JSON envelope in args[1]")
	}

	// ---- 1. parse envelope ----
	var env RestoreDumpEnvelope
	if err := json.Unmarshal([]byte(args[1]), &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}

	// Always scrub the encryption key bytes before returning, no matter
	// which exit path we take. The decrypter keeps its own derived AEAD
	// instance, not the raw key, so scrubbing is safe once we've handed
	// the bytes to NewStreamDecrypter.
	defer scrubBytes(env.EncryptionKey)

	target := env.TargetDatabase

	// ---- 2. validate ----
	if env.InstanceID == "" {
		return emitRestoreFailureAndError(emit, target, "instance_id is required")
	}
	if target == "" {
		return emitRestoreFailureAndError(emit, target, "target_database is required")
	}
	if !targetDatabaseRe.MatchString(target) {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("target_database %q rejected by whitelist (must match ^[a-zA-Z_][a-zA-Z0-9_]*$)", target))
	}
	if len(env.EncryptionKey) != keySize {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("encryption_key must be %d bytes (AES-256)", keySize))
	}
	if env.SourceObjectKey == "" {
		return emitRestoreFailureAndError(emit, target, "source_object_key is required")
	}
	// Bucket is mandatory for S3-style sources but explicitly empty for
	// Cloudnan Storage (the source is a local file the control plane
	// pre-pushed; there's no S3 endpoint). Validate per-provider.
	if env.Destination.Provider != "cloudnan" && env.Destination.Bucket == "" {
		return emitRestoreFailureAndError(emit, target, "destination is required")
	}

	// ---- 3. confirmation-token gate ----
	// Token target = the freshly-named restore database. The control
	// plane signs exactly that name; an attacker cannot redirect a
	// restore to a different DB without re-minting the token.
	if err := verifyOpToken(env.ConfirmationToken, "restore_dump", env.InstanceID, target); err != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("confirmation token: %v", err))
	}

	// ---- 4. resolve credentials + driver ----
	driver, db, cred, err := h.openInstance(ctx, env.InstanceID)
	if err != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("open instance: %v", err))
	}
	// We close the admin connection AFTER creating the target database;
	// the actual restore subprocess opens its own connection.
	defer func() { _ = db.Close() }()

	// Cross-check: the envelope's stated engine must match what the
	// vault recorded. A mismatch is either a control-plane bug or an
	// attempt to feed us the wrong dump format; refuse.
	if env.Engine != "" && env.Engine != cred.Engine {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("engine mismatch: envelope=%q vault=%q", env.Engine, cred.Engine))
	}

	restorer, err := restorerFor(driver.Engine())
	if err != nil {
		return emitRestoreFailureAndError(emit, target, err.Error())
	}

	// ---- 5. create the target database ----
	emitRestoreProgress(emit, &RestoreProgressEvent{
		Phase:          restorePhasePreparing,
		TargetDatabase: target,
	})

	if err := createRestoreTarget(ctx, driver.Engine(), db, target, env.OwnerUsername, env.OwnerHost); err != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("create target: %v", err))
	}

	// Track whether we should drop the target on failure. We only roll
	// back if the FAILURE happened during/after our create — a pre-create
	// failure has nothing to roll back. cleanup is set true once the
	// pipeline succeeds, suppressing the rollback.
	restoreSucceeded := false
	defer func() {
		if restoreSucceeded {
			return
		}
		// Fire-and-forget rollback: best-effort drop of the partially
		// populated target so a failed restore does not leave a half-
		// loaded database lying around for the operator to clean up.
		// Use a fresh context with a short deadline so a canceled
		// outer ctx does not also cancel the rollback.
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = dropRestoreTarget(dropCtx, driver.Engine(), db, target)
	}()

	// ---- 6. open ciphertext source ----
	// Branch by provider: S3-style downloads from the cloud bucket;
	// Cloudnan Storage opens a local file the control plane pre-pushed
	// to /tmp/cloudnan-dbrestore/<key>. The pipeline below
	// (decrypter → gunzip → restorer) is identical for both.
	var (
		source        readerSource
		totalEstimate uint64
	)
	if env.Destination.Provider == "cloudnan" {
		localPath := filepath.Join(localRestoreRoot, env.SourceObjectKey)
		lfs, err := NewLocalFileSource(localPath)
		if err != nil {
			return emitRestoreFailureAndError(emit, target, fmt.Sprintf("local source: %v", err))
		}
		source = lfs
		totalEstimate = lfs.TotalBytes()
	} else {
		downloader, err := NewChunkDownloader(ctx, &env.Destination, env.SourceObjectKey)
		if err != nil {
			return emitRestoreFailureAndError(emit, target, fmt.Sprintf("downloader: %v", err))
		}
		source = downloader
		totalEstimate = downloader.Total
	}
	defer func() { _ = source.Close() }()

	emitRestoreProgress(emit, &RestoreProgressEvent{
		Phase:              restorePhaseDownloading,
		BytesTotalEstimate: totalEstimate,
		TargetDatabase:     target,
	})

	// ---- 7. wire decrypter on top of source ----
	decrypter, err := NewStreamDecrypter(env.EncryptionKey, source)
	if err != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("decrypter: %v", err))
	}

	// ---- 8. optional gunzip on top of decrypter ----
	var pipelineHead io.Reader = decrypter
	var gzipReader *gzip.Reader
	if env.Compressed {
		gz, err := gzip.NewReader(decrypter)
		if err != nil {
			return emitRestoreFailureAndError(emit, target, fmt.Sprintf("gzip: %v", err))
		}
		gzipReader = gz
		pipelineHead = gz
	}
	// Close the gzip reader on every exit path to surface trailing-CRC
	// validation errors. If the cipher stream ended mid-gzip-frame, the
	// gzip reader's Read would have already returned an error; Close
	// only catches "valid frame, but truncated container" cases.
	defer func() {
		if gzipReader != nil {
			_ = gzipReader.Close()
		}
	}()

	// ---- 9. spawn restore tool ----
	program, restoreArgs, restoreEnv, err := restorer.Args(cred, target)
	if err != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("build restore args: %v", err))
	}

	// CommandContext kills the child on ctx cancel. This is our only
	// handle on the restore subprocess once Start() has fired.
	cmd := exec.CommandContext(ctx, program, restoreArgs...)
	cmd.Env = append(cmd.Environ(), restoreEnv...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("stdin pipe: %v", err))
	}
	stderrCap := newBoundedWriter(64 * 1024)
	cmd.Stderr = stderrCap

	if err := cmd.Start(); err != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("start %s: %v", program, err))
	}

	// ---- 10. emit RESTORING phase + start progress ticker ----
	emitRestoreProgress(emit, &RestoreProgressEvent{
		Phase:              restorePhaseRestoring,
		BytesTotalEstimate: totalEstimate,
		TargetDatabase:     target,
	})

	// pipedAtomic counts plaintext bytes successfully delivered to the
	// restore subprocess's stdin. We separately track the downloaded
	// (ciphertext) byte count via source.Downloaded(); the frontend
	// can compute a percentage off either, but ciphertext is the
	// canonical "how much of the S3 object have we consumed" and lines
	// up with totalEstimate, so that's what we report.
	var pipedAtomic atomic.Uint64
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
				emitRestoreProgress(emit, &RestoreProgressEvent{
					Phase:              restorePhaseRestoring,
					BytesProcessed:     source.Downloaded(),
					BytesTotalEstimate: totalEstimate,
					TargetDatabase:     target,
				})
			}
		}
	}()

	// ---- 11. pump bytes ----
	// io.CopyBuffer with a 1-MiB buffer aligns reads with the
	// decrypter's block size — every iteration drains exactly one
	// decrypted block to the restore subprocess's stdin.
	copyBuf := make([]byte, copyBufferSize)
	piped, copyErr := io.CopyBuffer(stdin, pipelineHead, copyBuf)
	pipedAtomic.Store(uint64(piped))

	// Close the gzip reader BEFORE closing stdin, so any deferred
	// validation error (truncated gzip trailer) is surfaced before we
	// signal EOF to the restore subprocess. A truncated dump fed to
	// pg_restore otherwise looks like a valid short input and aborts
	// with a less-clear error.
	if gzipReader != nil {
		if cerr := gzipReader.Close(); cerr != nil && copyErr == nil {
			copyErr = fmt.Errorf("gzip close: %w", cerr)
		}
		gzipReader = nil // suppress the deferred Close
	}

	// Closing stdin is what tells the restore subprocess "no more
	// input". Without this it would wait forever for more bytes.
	if cerr := stdin.Close(); cerr != nil && copyErr == nil {
		copyErr = fmt.Errorf("stdin close: %w", cerr)
	}

	// Wait for the restore subprocess to drain its input and exit. If
	// the subprocess was still consuming bytes when we hit copyErr, the
	// CommandContext-bound process group will get a SIGKILL when ctx
	// dies; otherwise Wait blocks until the child finishes processing
	// what we already piped.
	waitErr := cmd.Wait()

	// Stop the ticker now that no more bytes will flow. Wait for the
	// goroutine to exit so the final emit below races nothing.
	cancelTicker()
	<-tickerDone

	if copyErr != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("pipe copy: %v (restore stderr: %s)", copyErr, stderrCap.String()))
	}
	if waitErr != nil {
		return emitRestoreFailureAndError(emit, target, fmt.Sprintf("%s exit: %v (stderr: %s)", program, waitErr, stderrCap.String()))
	}

	// ---- 12. mark success + emit COMPLETED ----
	restoreSucceeded = true
	finalBytes := source.Downloaded()
	emitRestoreProgress(emit, &RestoreProgressEvent{
		Phase:              restorePhaseCompleted,
		BytesProcessed:     finalBytes,
		BytesTotalEstimate: totalEstimate,
		TargetDatabase:     target,
	})
	return nil
}

// ----- helpers -----

// emitRestoreProgress serializes msg as JSON + '\n' (NDJSON) and pushes
// it. Marshal failure is reported as a hand-built JSON error chunk; we
// never silently drop progress updates.
func emitRestoreProgress(emit func(string), msg *RestoreProgressEvent) {
	if emit == nil {
		return
	}
	b, err := json.Marshal(msg)
	if err != nil {
		emit(fmt.Sprintf(`{"phase":"failed","error_message":"json marshal: %s"}`+"\n", err.Error()))
		return
	}
	emit(string(b) + "\n")
}

// emitRestoreFailureAndError emits a failed phase chunk and returns an
// error carrying the same message. Used by every error exit path.
func emitRestoreFailureAndError(emit func(string), target, message string) error {
	emitRestoreProgress(emit, &RestoreProgressEvent{
		Phase:          restorePhaseFailed,
		TargetDatabase: target,
		ErrorMessage:   message,
	})
	return errors.New(message)
}

// createRestoreTarget creates the target database on the engine,
// optionally setting an owner. Refuses to operate on a name that
// already exists — the control plane is expected to mint a fresh
// `_restore_<timestamp>` suffix; if we see a collision, it is either
// a clock-skew bug or an indication that the operator is reusing a
// name and we must not silently overwrite anything.
func createRestoreTarget(ctx context.Context, engine pb.DatabaseEngine, db *sql.DB, target, ownerUser, ownerHost string) error {
	exists, err := databaseExists(ctx, engine, db, target)
	if err != nil {
		return fmt.Errorf("check existing: %w", err)
	}
	if exists {
		return fmt.Errorf("database %q already exists; refuse to overwrite", target)
	}

	switch engine {
	case pb.DatabaseEngine_DATABASE_ENGINE_MYSQL,
		pb.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		stmt := fmt.Sprintf(
			"CREATE DATABASE %s CHARACTER SET %s COLLATE %s",
			quoteMySQLIdent(target),
			defaultMySQLCharset,
			defaultMySQLCollation,
		)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("mysql create: %w", err)
		}
		if ownerUser != "" {
			host := ownerHost
			if host == "" {
				host = defaultMySQLHost
			}
			grant := fmt.Sprintf(
				"GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%s'",
				quoteMySQLIdent(target),
				escapeSQLString(ownerUser),
				escapeSQLString(host),
			)
			if _, err := db.ExecContext(ctx, grant); err != nil {
				// Roll back: if the grant fails the database exists
				// without an owner, which is a half-baked state.
				_, _ = db.ExecContext(ctx, "DROP DATABASE "+quoteMySQLIdent(target))
				return fmt.Errorf("mysql grant: %w", err)
			}
			if _, err := db.ExecContext(ctx, "FLUSH PRIVILEGES"); err != nil {
				return fmt.Errorf("mysql flush: %w", err)
			}
		}
		return nil

	case pb.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		// CREATE DATABASE in PG cannot run inside a transaction; the
		// driver's auto-commit on a single ExecContext is what we want.
		stmt := fmt.Sprintf(
			"CREATE DATABASE %s ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0",
			quotePostgresIdent(target),
		)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("postgres create: %w", err)
		}
		if ownerUser != "" {
			alter := fmt.Sprintf(
				"ALTER DATABASE %s OWNER TO %s",
				quotePostgresIdent(target),
				quotePostgresIdent(ownerUser),
			)
			if _, err := db.ExecContext(ctx, alter); err != nil {
				_, _ = db.ExecContext(ctx, "DROP DATABASE "+quotePostgresIdent(target))
				return fmt.Errorf("postgres alter owner: %w", err)
			}
		}
		return nil

	default:
		return fmt.Errorf("unsupported engine %v", engine)
	}
}

// dropRestoreTarget is the rollback companion to createRestoreTarget.
// Best-effort: errors are returned but the orchestrator ignores them
// (a failed rollback only matters to operator hygiene; the failure path
// already returns the original error).
func dropRestoreTarget(ctx context.Context, engine pb.DatabaseEngine, db *sql.DB, target string) error {
	switch engine {
	case pb.DatabaseEngine_DATABASE_ENGINE_MYSQL,
		pb.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		_, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteMySQLIdent(target))
		return err
	case pb.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		// Terminate any backends lingering in the target before drop.
		// pg_restore opens its own backend; if we are rolling back
		// because pg_restore exited mid-stream, the backend should
		// already be closed, but defensively terminate any stragglers.
		if err := terminatePostgresBackends(ctx, db, target); err != nil {
			// Continue to DROP regardless — the DROP itself will
			// surface a clearer "database is being accessed" error if
			// termination really failed.
			_ = err
		}
		_, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quotePostgresIdent(target))
		return err
	default:
		return fmt.Errorf("unsupported engine %v", engine)
	}
}

// databaseExists returns true if a database with the given name is
// present on the connected instance. Engine-specific because the
// catalog layout differs.
func databaseExists(ctx context.Context, engine pb.DatabaseEngine, db *sql.DB, name string) (bool, error) {
	switch engine {
	case pb.DatabaseEngine_DATABASE_ENGINE_MYSQL,
		pb.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		var n int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?",
			name,
		).Scan(&n)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	case pb.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		var n int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pg_database WHERE datname = $1",
			name,
		).Scan(&n)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	default:
		return false, fmt.Errorf("unsupported engine %v", engine)
	}
}
