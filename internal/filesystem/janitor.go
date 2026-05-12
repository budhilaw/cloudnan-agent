package filesystem

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// StartBackupJanitor runs a background goroutine that prunes the AI
// backup directory. Two policies, both enforced every tick:
//
//   - Drop any per-invocation directory older than MaxBackupAge.
//   - If the total size of all remaining directories exceeds
//     MaxBackupTotalBytes, evict the oldest first until back under
//     the cap.
//
// First tick fires ~5 minutes after agent start (lets the system
// settle); subsequent ticks every 6 hours. The agent process is
// long-lived (systemd-managed), so this runs for the lifetime of
// the agent.
//
// Eviction is logged to stderr so the operator can correlate with
// "Undo no longer available" errors in the panel — the rollback
// recipe still points at a path that no longer exists, and the BE's
// Undo button will surface a clean error on click.
func StartBackupJanitor(root string) {
	if root == "" {
		root = BackupRoot
	}
	go func() {
		// Initial delay so the first tick doesn't compete with
		// startup churn.
		time.Sleep(5 * time.Minute)
		runJanitorCycle(root)
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			runJanitorCycle(root)
		}
	}()
}

func runJanitorCycle(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[BackupJanitor] read %q: %v", root, err)
		}
		return
	}
	type item struct {
		path string
		mod  time.Time
		size int64
	}
	now := time.Now()
	var keep []item
	var totalBytes int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(root, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Age-based eviction first.
		if now.Sub(info.ModTime()) > MaxBackupAge {
			size, _, _ := dirSize(full)
			if rmErr := os.RemoveAll(full); rmErr != nil {
				log.Printf("[BackupJanitor] failed to evict %q: %v", full, rmErr)
				continue
			}
			log.Printf("[BackupJanitor] evicted (age) %q (%d bytes)", full, size)
			continue
		}
		size, _, _ := dirSize(full)
		totalBytes += size
		keep = append(keep, item{path: full, mod: info.ModTime(), size: size})
	}
	// Size-cap eviction: drop oldest until under the cap.
	if totalBytes <= MaxBackupTotalBytes {
		return
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].mod.Before(keep[j].mod) })
	for _, it := range keep {
		if totalBytes <= MaxBackupTotalBytes {
			break
		}
		if rmErr := os.RemoveAll(it.path); rmErr != nil {
			log.Printf("[BackupJanitor] failed to evict %q: %v", it.path, rmErr)
			continue
		}
		totalBytes -= it.size
		log.Printf("[BackupJanitor] evicted (cap) %q (%d bytes)", it.path, it.size)
	}
}
