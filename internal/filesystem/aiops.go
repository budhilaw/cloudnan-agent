// Package filesystem also exposes a hardened surface for AI-driven
// file mutations. Distinct from the general-purpose Manager used by
// the file browser:
//
//   - Path blocklist enforced at the executor (defense in depth — the
//     control plane also blocks at the tool spec, but the agent must
//     refuse anyway, in case the control plane is ever compromised).
//   - Symlink-safe resolution: the final target is re-validated after
//     EvalSymlinks so a symlink under /tmp can't redirect into /etc.
//   - Every mutation is preceded by a backup capture: the original
//     file (or directory tar) is copied into
//     /var/backups/cloudnan/<invocation_id>/ before any write/delete,
//     and the rollback recipe in the returned BackupInfo restores it
//     byte-for-byte (mode + owner preserved).
//   - Size caps so a runaway tool can't fill the disk.
package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// BackupRoot is where per-invocation snapshots live. Each AI mutation
// gets its own subdirectory keyed by invocation_id; the rollback
// recipe references files inside it. Configurable via env so tests
// (and Docker images mounting a different host fs) can override.
var BackupRoot = func() string {
	if v := os.Getenv("CLOUDNAN_BACKUP_ROOT"); v != "" {
		return v
	}
	return "/var/backups/cloudnan"
}()

// Hard size caps. Picked to be generous enough for real config-file
// edits (nginx vhosts, systemd units, logrotate.d entries) while
// keeping a runaway tool from filling the disk in one call.
const (
	MaxWriteBytes      = 10 * 1024 * 1024        // 10 MiB per write
	MaxDeleteDirBytes  = int64(1 * 1024 * 1024 * 1024) // 1 GiB total per dir delete
	MaxDeleteDirFiles  = 50_000
	MaxBackupAge       = 30 * 24 * time.Hour
	MaxBackupTotalBytes = int64(1 * 1024 * 1024 * 1024) // 1 GiB across all backups
)

// blockedPathPrefixes are paths the AI is NEVER allowed to mutate,
// regardless of what the LLM (or even a compromised control plane)
// requests. Reads are also blocked since reading some of these would
// leak credentials. Match by prefix on the cleaned absolute path.
var blockedPathPrefixes = []string{
	"/etc/shadow",
	"/etc/gshadow",
	"/etc/sudoers",
	"/etc/sudoers.d/",
	"/etc/pam.d/",
	"/etc/ssh/ssh_host_",
	"/root/.ssh/",
	"/boot/",
	"/dev/",
	"/proc/",
	"/sys/",
	"/lib/modules/",
	"/usr/lib/modules/",
	"/opt/cloudnan/agent/", // the agent's own install dir
}

// blockedNameSuffixes — block by basename so /home/*/.ssh/authorized_keys,
// id_rsa, etc. are all rejected wherever they live.
var blockedNameSuffixes = []string{
	"/.ssh/authorized_keys",
	"/.ssh/id_rsa",
	"/.ssh/id_ed25519",
	"/.ssh/id_ecdsa",
	"/.ssh/id_dsa",
}

// BackupInfo describes a snapshot the agent captured before mutating
// the filesystem. It's serialized into the agent's CommandResponse
// stdout (JSON) so the control plane can persist it as the
// invocation's rollback_recipe and Undo metadata.
type BackupInfo struct {
	// InvocationID echoed back so the response is self-describing
	// even when the channel multiplexes multiple commands.
	InvocationID string `json:"invocation_id"`
	// AbsPath of the original target.
	AbsPath string `json:"abs_path"`
	// BackupPath is where the original content sits on disk now.
	// Empty when OriginalAbsent is true (no need to back up a file
	// that didn't exist; rollback is just an `rm`).
	BackupPath string `json:"backup_path,omitempty"`
	// OriginalAbsent is true when the target didn't exist before the
	// mutation (a fresh create). Rollback for these is `rm -f`.
	OriginalAbsent bool `json:"original_absent"`
	// IsDir indicates the backup is a tar.gz of a directory.
	IsDir bool `json:"is_dir"`
	// Mode + UID + GID of the original — needed to chmod/chown the
	// restored file back to its original ownership.
	Mode uint32 `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
	// SHA256 of the original content (file only). Lets the FE show
	// "before/after" hashes and helps detect tampering of the
	// backup blob.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size"`
	// Recipe is the shell command that restores the original. The
	// existing rollback-recipe / Undo flow runs this verbatim — no
	// new BE wiring required.
	Recipe string `json:"recipe"`
	// CapturedAt is when the snapshot was taken (RFC3339).
	CapturedAt string `json:"captured_at"`
}

// AIWriteRequest, AIEditRequest, etc. — the typed inputs the
// agent.go dispatcher passes into the methods below.
type AIWriteRequest struct {
	InvocationID string
	AbsPath      string
	Content      []byte
	// Mode is the file mode to set (0 = keep existing or use 0644
	// for new files).
	Mode uint32
}

type AIEditRequest struct {
	InvocationID string
	AbsPath      string
	OldString    string
	NewString    string
	// ReplaceAll: when false (default), refuse if old_string occurs
	// more than once — forces the LLM to give enough context for a
	// unique match. When true, replace every occurrence.
	ReplaceAll bool
}

type AIDeleteRequest struct {
	InvocationID string
	AbsPath      string
	// Recursive: required when target is a directory. Refuses
	// otherwise even if the LLM passed --recursive style flags.
	Recursive bool
}

// AIWrite creates or overwrites a file. Returns BackupInfo with the
// recipe to restore the previous content (or `rm` for fresh
// creates).
func (m *Manager) AIWrite(req AIWriteRequest) (*BackupInfo, error) {
	if req.InvocationID == "" {
		return nil, errors.New("invocation_id required")
	}
	if len(req.Content) > MaxWriteBytes {
		return nil, fmt.Errorf("write rejected: content is %d bytes, limit is %d", len(req.Content), MaxWriteBytes)
	}
	abs, err := m.resolveForAI(req.AbsPath, true /*allow non-existent*/)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, fmt.Errorf("mkdir parent: %w", err)
	}
	// Capture the pre-mutation state.
	info, captureErr := m.captureBackup(req.InvocationID, abs)
	if captureErr != nil {
		return nil, captureErr
	}
	// Pre-flight disk space check: refuse if free space < 2x content
	// (we need room for the backup + the new file, with some slack).
	if err := m.assertFreeSpace(parent, int64(len(req.Content))*2); err != nil {
		return nil, err
	}
	// Atomic write: stage to a temp file in the same directory, then
	// rename. Same-directory rename guarantees atomicity on POSIX.
	tmp, err := os.CreateTemp(parent, ".cloudnan-write-*")
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(req.Content); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return nil, err
	}
	// Preserve existing mode if the file already existed, otherwise
	// 0644 (or req.Mode if explicit).
	mode := os.FileMode(0644)
	if req.Mode != 0 {
		mode = os.FileMode(req.Mode)
	} else if !info.OriginalAbsent {
		mode = os.FileMode(info.Mode)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return nil, fmt.Errorf("chmod temp: %w", err)
	}
	// Preserve existing owner. We only chown when running with the
	// privileges to do so; ignore errors otherwise so a user-mode
	// agent doesn't fail on this step.
	if !info.OriginalAbsent {
		_ = os.Chown(tmpPath, int(info.UID), int(info.GID))
	}
	if err := os.Rename(tmpPath, abs); err != nil {
		cleanup()
		return nil, fmt.Errorf("rename into place: %w", err)
	}
	return info, nil
}

// AIEdit performs a find-and-replace edit against a file. Captures a
// backup before mutating. Refuses when old_string is ambiguous
// (multiple matches) unless ReplaceAll is set, so the LLM is forced
// to provide enough surrounding context for a unique match.
func (m *Manager) AIEdit(req AIEditRequest) (*BackupInfo, error) {
	if req.InvocationID == "" {
		return nil, errors.New("invocation_id required")
	}
	if req.OldString == "" {
		return nil, errors.New("old_string required")
	}
	if req.OldString == req.NewString {
		return nil, errors.New("old_string == new_string, nothing to edit")
	}
	abs, err := m.resolveForAI(req.AbsPath, false)
	if err != nil {
		return nil, err
	}
	original, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read original: %w", err)
	}
	count := strings.Count(string(original), req.OldString)
	if count == 0 {
		return nil, errors.New("old_string not found in file")
	}
	if count > 1 && !req.ReplaceAll {
		return nil, fmt.Errorf("old_string matches %d places — pass replace_all=true or include more context to disambiguate", count)
	}
	var updated string
	if req.ReplaceAll {
		updated = strings.ReplaceAll(string(original), req.OldString, req.NewString)
	} else {
		updated = strings.Replace(string(original), req.OldString, req.NewString, 1)
	}
	if len(updated) > MaxWriteBytes {
		return nil, fmt.Errorf("edit rejected: result is %d bytes, limit is %d", len(updated), MaxWriteBytes)
	}
	// Reuse AIWrite for the atomic-rename + backup pattern. The
	// backup snapshot it captures is the same `original` we just
	// read, so rollback is exact.
	return m.AIWrite(AIWriteRequest{
		InvocationID: req.InvocationID,
		AbsPath:      req.AbsPath,
		Content:      []byte(updated),
	})
}

// AIDelete removes a file or a directory tree. Captures a backup
// (full file copy, or tar.gz for directories) so Undo restores the
// original byte-for-byte including mode + ownership.
func (m *Manager) AIDelete(req AIDeleteRequest) (*BackupInfo, error) {
	if req.InvocationID == "" {
		return nil, errors.New("invocation_id required")
	}
	abs, err := m.resolveForAI(req.AbsPath, false)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat target: %w", err)
	}
	if st.IsDir() && !req.Recursive {
		return nil, errors.New("target is a directory; pass recursive=true to delete it")
	}
	info, err := m.captureBackup(req.InvocationID, abs)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		if err := os.RemoveAll(abs); err != nil {
			return nil, fmt.Errorf("remove dir: %w", err)
		}
	} else {
		if err := os.Remove(abs); err != nil {
			return nil, fmt.Errorf("remove file: %w", err)
		}
	}
	return info, nil
}

// resolveForAI normalizes the path, blocks the AI surface, and
// enforces symlink-safety: if the final target resolves through a
// symlink, the RESOLVED path is re-checked against the blocklist so
// a writable symlink can't redirect into /etc/shadow.
//
// allowMissing lets AIWrite create a fresh file; AIEdit/AIDelete
// require the target to exist.
func (m *Manager) resolveForAI(input string, allowMissing bool) (string, error) {
	if input == "" {
		return "", errors.New("path required")
	}
	if !filepath.IsAbs(input) {
		return "", errors.New("path must be absolute")
	}
	cleaned := filepath.Clean(input)
	if err := checkBlocked(cleaned); err != nil {
		return "", err
	}
	// Resolve symlinks IFF the target exists. EvalSymlinks fails on
	// non-existent paths so we tolerate that for creates.
	resolved := cleaned
	if r, err := filepath.EvalSymlinks(cleaned); err == nil {
		resolved = r
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	} else if !allowMissing {
		return "", err
	}
	// Re-check the resolved path so a symlink under a writable dir
	// can't smuggle in a blocked target.
	if err := checkBlocked(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// checkBlocked returns nil when the path is allowed, an error
// otherwise. Match logic: prefix on absolute paths + basename
// suffix. The agent enforces this regardless of what the control
// plane sent, so a compromised core can't bypass it.
func checkBlocked(abs string) error {
	for _, p := range blockedPathPrefixes {
		if abs == strings.TrimSuffix(p, "/") || strings.HasPrefix(abs, p) {
			return fmt.Errorf("path %q is blocked from AI access by the agent", abs)
		}
	}
	for _, s := range blockedNameSuffixes {
		if strings.HasSuffix(abs, s) {
			return fmt.Errorf("path %q is blocked from AI access by the agent", abs)
		}
	}
	return nil
}

// captureBackup snapshots the original contents (or absence) of abs
// into BackupRoot/<invocation_id>/. Returns a BackupInfo with the
// rollback recipe.
func (m *Manager) captureBackup(invocationID, abs string) (*BackupInfo, error) {
	dir := filepath.Join(BackupRoot, invocationID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir backup dir: %w", err)
	}
	st, err := os.Lstat(abs)
	info := &BackupInfo{
		InvocationID: invocationID,
		AbsPath:      abs,
		CapturedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	switch {
	case err != nil && errors.Is(err, os.ErrNotExist):
		info.OriginalAbsent = true
		info.Recipe = fmt.Sprintf("rm -f %s", shellQuote(abs))
		return info, nil
	case err != nil:
		return nil, fmt.Errorf("stat for backup: %w", err)
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); ok {
		info.Mode = uint32(st.Mode().Perm())
		info.UID = stat.Uid
		info.GID = stat.Gid
	} else {
		info.Mode = uint32(st.Mode().Perm())
	}
	info.Size = st.Size()
	// Refuse to back up massive trees that would blow the disk
	// budget on their own.
	if st.IsDir() {
		info.IsDir = true
		total, fileCount, err := dirSize(abs)
		if err != nil {
			return nil, fmt.Errorf("size dir for backup: %w", err)
		}
		if total > MaxDeleteDirBytes {
			return nil, fmt.Errorf("directory is %d bytes, refusing to backup+delete trees larger than %d", total, MaxDeleteDirBytes)
		}
		if fileCount > MaxDeleteDirFiles {
			return nil, fmt.Errorf("directory has %d files, refusing trees larger than %d", fileCount, MaxDeleteDirFiles)
		}
		info.Size = total
		backupPath := filepath.Join(dir, filepath.Base(abs)+".tar.gz")
		if err := tarGzDir(abs, backupPath); err != nil {
			return nil, fmt.Errorf("backup dir: %w", err)
		}
		info.BackupPath = backupPath
		info.Recipe = fmt.Sprintf(
			"mkdir -p %s && tar -xzf %s -C %s",
			shellQuote(filepath.Dir(abs)),
			shellQuote(backupPath),
			shellQuote(filepath.Dir(abs)),
		)
		return info, nil
	}
	// Regular file backup: copy contents + preserve mode + record sha256.
	backupPath := filepath.Join(dir, filepath.Base(abs))
	if err := copyFilePreservingMode(abs, backupPath); err != nil {
		return nil, fmt.Errorf("backup file: %w", err)
	}
	sum, err := sha256File(abs)
	if err == nil {
		info.SHA256 = sum
	}
	info.BackupPath = backupPath
	info.Recipe = fmt.Sprintf(
		"install -m %o -o %d -g %d %s %s",
		info.Mode, info.UID, info.GID,
		shellQuote(backupPath), shellQuote(abs),
	)
	return info, nil
}

// assertFreeSpace returns an error when the filesystem hosting dir
// has less than need bytes available. Best-effort: failures fall
// open (better than blocking real work on a flaky statvfs).
func (m *Manager) assertFreeSpace(dir string, need int64) error {
	if need <= 0 {
		return nil
	}
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		log.Printf("[FileSystem] assertFreeSpace: statfs %q: %v (allowing)", dir, err)
		return nil
	}
	avail := int64(s.Bavail) * int64(s.Bsize)
	if avail < need {
		return fmt.Errorf("insufficient disk space: need %d bytes, have %d", need, avail)
	}
	return nil
}

// shellQuote returns a single-quoted shell-safe rendering of s. We
// only ever join paths and integers into recipe strings; this is
// enough for those, no parsing of arbitrary user input.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func copyFilePreservingMode(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func dirSize(root string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			count++
		}
		return nil
	})
	return total, count, err
}

func tarGzDir(src, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	gz := gzip.NewWriter(out)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()
	base := filepath.Dir(src)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return relErr
		}
		var linkTarget string
		if info.Mode()&os.ModeSymlink != 0 {
			t, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			linkTarget = t
		}
		hdr, hdrErr := tar.FileInfoHeader(info, linkTarget)
		if hdrErr != nil {
			return hdrErr
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			_, copyErr := io.Copy(tw, f)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	})
}
