package swarm

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type Store struct {
	Root string
}

func NewStore(root string) *Store {
	return &Store{Root: root}
}

func (s *Store) EnsureDir(parts ...string) string {
	p := filepath.Join(append([]string{s.Root}, parts...)...)
	_ = os.MkdirAll(p, 0755)
	return p
}

func (s *Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func (s *Store) WriteJSON(path string, v interface{}) error {
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0755)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) ReadJSON(path string, v interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (s *Store) Exists(parts ...string) bool {
	_, err := os.Stat(s.Path(parts...))
	return err == nil
}

func (s *Store) ListJSONFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

func (s *Store) Remove(path string) error {
	return os.Remove(path)
}

// WithLock acquires a global flock for cross-process safety.
func (s *Store) WithLock(fn func() error) error {
	lockPath := s.Path(".global.lock")
	_ = os.MkdirAll(filepath.Dir(lockPath), 0755)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open global lock: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}

func PathHash(file string) string {
	h := sha256.Sum256([]byte(filepath.Clean(file)))
	return fmt.Sprintf("%x", h[:8])
}
