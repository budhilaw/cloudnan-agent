// Package database — source.go.
//
// readerSource is the abstraction the restore-dump pipeline reads
// ciphertext from. Two concrete implementations:
//
//   - ChunkDownloader — S3-style (S3 / R2 / Wasabi / B2 / MinIO / DO
//                       Spaces / Linode / Vultr). Default.
//   - LocalFileSource — Cloudnan Storage. Before dispatching the
//                       restore command, the control plane reads the
//                       encrypted dump off /var/cloudnan/backups,
//                       chunks it via the EXEC FILE write helper to
//                       /tmp/cloudnan-dbrestore/<key> on this agent.
//                       LocalFileSource just opens that local path.
//
// Why mirror sink.go: the restore pipeline (source → decrypter →
// gunzip → restorer) is otherwise byte-for-byte identical regardless
// of where the ciphertext came from. Branching once at source
// construction time keeps the rest of the code path single-impl.
package database

import (
	"errors"
	"fmt"
	"os"
)

// readerSource is the contract every restore source must satisfy.
// Total reports the on-source size in bytes (used for the progress
// bar's known-total). Read + Close mirror io.ReadCloser. Downloaded
// is sampled by the progress ticker for live byte counts during the
// restore.
type readerSource interface {
	Read(p []byte) (int, error)
	Close() error
	// TotalBytes returns the total ciphertext size, or 0 if unknown.
	TotalBytes() uint64
	// Downloaded returns the running total of bytes successfully read
	// by the consumer. Mirrors ChunkDownloader.Downloaded() so the
	// progress ticker is sink-agnostic.
	Downloaded() uint64
}

// LocalFileSource reads ciphertext from a local file on the agent
// host. The control plane is responsible for putting the file there
// (chunked push from /var/cloudnan/backups before the restore
// command fires) and for cleanup post-restore.
type LocalFileSource struct {
	path string
	f    *os.File
	size uint64
	// read tracks bytes successfully delivered to Read() callers;
	// sampled by the orchestrator's progress ticker.
	read uint64
}

// NewLocalFileSource opens path read-only. A failure here means the
// control plane's pre-push step did not land the file — usually a
// chunked-upload error or a fingerprint mismatch — and the restore
// must fail fast rather than crash later inside the decrypter.
func NewLocalFileSource(path string) (*LocalFileSource, error) {
	if path == "" {
		return nil, errors.New("local file source: empty path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("local file source: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("local file source: stat %s: %w", path, err)
	}
	if st.Size() < 0 {
		_ = f.Close()
		return nil, fmt.Errorf("local file source: negative size %d", st.Size())
	}
	return &LocalFileSource{path: path, f: f, size: uint64(st.Size())}, nil
}

func (s *LocalFileSource) Read(p []byte) (int, error) {
	n, err := s.f.Read(p)
	if n > 0 {
		s.read += uint64(n)
	}
	return n, err
}

// Downloaded returns the running total of bytes Read() has successfully
// delivered to the decrypter. Sampled by the progress ticker.
func (s *LocalFileSource) Downloaded() uint64 { return s.read }

// Close closes the file. The agent does NOT remove the on-disk file
// here — the control plane owns the lifecycle and issues an explicit
// cleanup EXEC after the restore completes (so a successful restore
// without a working cleanup still leaves /tmp tidy on next reboot).
func (s *LocalFileSource) Close() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

func (s *LocalFileSource) TotalBytes() uint64 { return s.size }
