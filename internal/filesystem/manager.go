package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

type FileType string

const (
	FileTypeFile      FileType = "file"
	FileTypeDirectory FileType = "directory"
)

// FileEntry mirrors the structure expected by the Core
type FileEntry struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Type    FileType `json:"type"`
	Size    int64    `json:"size"`
	Mode    string   `json:"mode"`
	ModTime int64    `json:"mod_time"`
	UID     uint32   `json:"uid"`
	GID     uint32   `json:"gid"`
}

type Manager struct {
	rootDir string
}

func New(rootDir string) *Manager {
	if rootDir == "" {
		rootDir = "/"
	}
	return &Manager{rootDir: rootDir}
}

func (m *Manager) resolvePath(p string) (string, error) {
	// Simple join. Add sandboxing logic here if needed later.
	return filepath.Join(m.rootDir, p), nil
}

func (m *Manager) List(path string) ([]*FileEntry, error) {
	fullPath, err := m.resolvePath(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return nil, err
	}

	var result []*FileEntry
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		fileType := FileTypeFile
		if entry.IsDir() {
			fileType = FileTypeDirectory
		}

		var uid, gid uint32
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			uid = stat.Uid
			gid = stat.Gid
		}

		result = append(result, &FileEntry{
			Name:    entry.Name(),
			Path:    filepath.Join(path, entry.Name()),
			Type:    fileType,
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			ModTime: info.ModTime().Unix(),
			UID:     uid,
			GID:     gid,
		})
	}

	// Sort: Directories first, then files
	sort.Slice(result, func(i, j int) bool {
		if result[i].Type != result[j].Type {
			return result[i].Type == FileTypeDirectory
		}
		return result[i].Name < result[j].Name
	})

	return result, nil
}

func (m *Manager) Read(path string) (string, error) {
	fullPath, err := m.resolvePath(path)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func (m *Manager) Write(path string, content string) error {
	fullPath, err := m.resolvePath(path)
	if err != nil {
		return err
	}
	// 0644 is standard
	return os.WriteFile(fullPath, []byte(content), 0644)
}

func (m *Manager) Delete(path string) error {
	fullPath, err := m.resolvePath(path)
	if err != nil {
		return err
	}
	return os.RemoveAll(fullPath)
}

func (m *Manager) Mkdir(path string) error {
	fullPath, err := m.resolvePath(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(fullPath, 0755)
}

func (m *Manager) Copy(source, dest string) error {
	srcPath, err := m.resolvePath(source)
	if err != nil {
		return err
	}
	dstPath, err := m.resolvePath(dest)
	if err != nil {
		return err
	}

	sourceFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	info, err := sourceFile.Stat()
	if err != nil {
		return err
	}

	if info.IsDir() {
		// Recursive copy not implemented effectively for MVP
		return fmt.Errorf("directory copy not supported yet")
	}

	destFile, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, sourceFile); err != nil {
		return err
	}

	return os.Chmod(dstPath, info.Mode())
}
