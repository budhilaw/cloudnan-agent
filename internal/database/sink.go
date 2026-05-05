// Package database — sink.go.
//
// uploadSink is the abstraction the export-dump pipeline writes its
// encrypted ciphertext into. Two concrete implementations:
//
//   - ChunkUploader  — S3-multipart uploads (S3 / R2 / Wasabi / B2 /
//                      MinIO / DO Spaces / Linode / Vultr). Default.
//   - LocalFileSink  — Cloudnan Storage. The control plane fetches the
//                      written file off the agent's local disk and
//                      writes it to /var/cloudnan/backups via the
//                      provider's UploadCiphertext path. Used when the
//                      destination has no S3 endpoint (Cloudnan Storage
//                      is a server-local filesystem, not a cloud
//                      bucket).
//
// Why split: the export pipeline (dump → gzip → encrypter → SINK) is
// otherwise byte-for-byte identical regardless of where the ciphertext
// lands. Branching on Provider once at sink construction time keeps
// the rest of the code path single-implementation.
package database

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// uploadSink is the contract every export sink must satisfy. It is
// intentionally narrow: the encrypter writes into Write, the export
// orchestrator calls Close on success or Abort on failure, and the
// progress ticker reads Uploaded.
//
// Both methods are safe to call after Write returns an error — Close
// turns into a no-op, Abort tears down whatever partial state exists.
//
// Implementations need NOT be safe for concurrent use; the export
// pipeline is single-goroutine on the writer side and the progress
// goroutine only reads via Uploaded.
type uploadSink interface {
	io.WriteCloser

	// Uploaded returns the running total of plaintext bytes successfully
	// committed to the destination. For ChunkUploader this is bytes
	// shipped to S3; for LocalFileSink this is bytes written to disk.
	Uploaded() uint64

	// Abort tears down any partial state (S3 multipart abort, partial
	// local file removal). Idempotent. Safe to call after Close — in
	// the success path the export orchestrator records `aborted=true`
	// and the deferred Abort is a no-op.
	Abort() error
}

// LocalFileSink writes ciphertext bytes to a local file on the agent
// host. Used by the Cloudnan Storage path: the control plane is
// responsible for fetching this file off the agent after COMPLETED is
// emitted and persisting it to its own disk.
//
// The file is created with mode 0600 because the encrypted dump is
// sensitive even on the agent's own host — only the cloudnan-agent
// service should be able to open it. A permissions slip-up here would
// expose the dump to other users on the same VPS, defeating the
// per-dump E2E key.
type LocalFileSink struct {
	path string

	mu       sync.Mutex
	f        *os.File
	written  uint64
	closed   bool
	aborted  bool
	writeErr error // sticky error from a failed Write, surfaced by Close
}

// NewLocalFileSink creates the parent directory (0700) and opens the
// file for writing (0600, truncating any prior content). The caller
// must call Close() in the success path or Abort() on error; failing
// to do either leaves a stale file under /tmp, which the control plane
// CleanupAgentTempFiles step is responsible for sweeping in any case.
func NewLocalFileSink(path string) (*LocalFileSink, error) {
	if path == "" {
		return nil, errors.New("local file sink: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("local file sink: mkdir parent: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("local file sink: open: %w", err)
	}
	return &LocalFileSink{path: path, f: f}, nil
}

// Path returns the on-disk path the sink is writing to. The export
// orchestrator surfaces this as the COMPLETED phase's object_key so
// the control plane can locate the file for fetch.
func (s *LocalFileSink) Path() string {
	return s.path
}

// Write appends p to the underlying file. Returns the number of bytes
// written and any I/O error. After a non-nil error is returned, all
// subsequent Writes are short-circuited to the same error so the
// pipeline doesn't double-report.
func (s *LocalFileSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("local file sink: write on closed sink")
	}
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	n, err := s.f.Write(p)
	if n > 0 {
		s.written += uint64(n)
	}
	if err != nil {
		s.writeErr = err
	}
	return n, err
}

// Close fsyncs the file (so a crash between here and the control
// plane's fetch doesn't lose data) and closes the descriptor. Any
// sticky write error surfaces here.
func (s *LocalFileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.writeErr != nil {
		// Still try to close the FD; report the original write error.
		_ = s.f.Close()
		return s.writeErr
	}
	if err := s.f.Sync(); err != nil {
		_ = s.f.Close()
		return fmt.Errorf("local file sink: fsync: %w", err)
	}
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("local file sink: close: %w", err)
	}
	return nil
}

// Uploaded returns plaintext bytes committed so far. For LocalFileSink
// "committed" means "written to the kernel" — fsync only happens on
// Close, so a kernel-level crash before Close could lose recent bytes.
// The export orchestrator treats this as best-effort progress, not as
// a durability guarantee.
func (s *LocalFileSink) Uploaded() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// Abort closes the FD (best-effort) and removes the partial file so
// the next attempt starts fresh. Idempotent. After Abort, the file
// path no longer exists; callers that race a fetch against an Abort
// must coordinate at the orchestrator level (we don't lock across
// processes).
func (s *LocalFileSink) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return nil
	}
	s.aborted = true
	if !s.closed {
		_ = s.f.Close()
		s.closed = true
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("local file sink: remove %s: %w", s.path, err)
	}
	return nil
}
