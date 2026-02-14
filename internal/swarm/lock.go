package swarm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type LockService struct {
	store *Store
	trace *TraceService
}

func NewLockService(store *Store, trace *TraceService) *LockService {
	return &LockService{store: store, trace: trace}
}

// LockFiles acquires lease-based locks on multiple files atomically.
// Files are sorted to avoid deadlock. On partial failure, all acquired locks are released.
// If wait_sec > 0, retries with backoff until timeout.
func (s *LockService) LockFiles(taskID, owner string, files []string, ttlSec, waitSec int) (*Lease, error) {
	if owner == "" || len(files) == 0 {
		return nil, fmt.Errorf("owner and files are required")
	}
	if ttlSec <= 0 {
		ttlSec = 120
	}
	if waitSec < 0 {
		waitSec = 0
	}

	// Normalize and sort files to prevent deadlock
	normalized := make([]string, len(files))
	for i, f := range files {
		normalized[i] = filepath.Clean(f)
	}
	sort.Strings(normalized)

	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	backoff := 500 * time.Millisecond

	for {
		lease, err := s.tryLockFiles(taskID, owner, normalized, ttlSec)
		if err == nil {
			return lease, nil
		}

		if time.Now().After(deadline) {
			s.trace.Log(TraceEvent{
				Type:    EventLockFailed,
				Actor:   owner,
				Subject: strings.Join(normalized, ", "),
				Detail:  err.Error(),
			})
			return nil, err
		}

		time.Sleep(backoff)
		if backoff < 4*time.Second {
			backoff = backoff * 3 / 2
		}
	}
}

func (s *LockService) tryLockFiles(taskID, owner string, files []string, ttlSec int) (*Lease, error) {
	var acquired []string
	leaseID := ""

	err := s.store.WithLock(func() error {
		now := time.Now().UTC()
		expiresAt := now.Add(time.Duration(ttlSec) * time.Second)

		leaseID = GenID("l")

		for _, file := range files {
			hash := PathHash(file)
			lockPath := s.store.Path("locks", "files", hash+".json")

			// Check existing lock
			var existing FileLock
			if err := s.store.ReadJSON(lockPath, &existing); err == nil {
				// Lock exists - check if expired
				expTime, _ := time.Parse(time.RFC3339, existing.ExpiresAt)
				if now.Before(expTime) {
					// Still valid and not ours
					if existing.Owner != owner {
						// Release all acquired locks
						for _, af := range acquired {
							ah := PathHash(af)
							_ = s.store.Remove(s.store.Path("locks", "files", ah+".json"))
						}
						return fmt.Errorf("file '%s' locked by '%s' (task: %s, expires: %s)",
							file, existing.Owner, existing.TaskID, existing.ExpiresAt)
					}
					// Same owner - reentrant, update the lock
				} else {
					// Expired - takeover
					s.trace.Log(TraceEvent{
						Type:    EventLockExpired,
						Actor:   owner,
						Subject: file,
						Detail:  fmt.Sprintf("took over expired lock from '%s'", existing.Owner),
					})
				}
			}

			lock := FileLock{
				LeaseID:       leaseID,
				Owner:         owner,
				TaskID:        taskID,
				File:          file,
				AcquiredAt:    now.Format(time.RFC3339),
				ExpiresAt:     expiresAt.Format(time.RFC3339),
				LastHeartbeat: now.Format(time.RFC3339),
			}

			s.store.EnsureDir("locks", "files")
			if err := s.store.WriteJSON(lockPath, &lock); err != nil {
				// Rollback
				for _, af := range acquired {
					ah := PathHash(af)
					_ = s.store.Remove(s.store.Path("locks", "files", ah+".json"))
				}
				return err
			}
			acquired = append(acquired, file)
		}

		// Write lease index
		lease := &Lease{
			LeaseID:       leaseID,
			Owner:         owner,
			TaskID:        taskID,
			Files:         files,
			AcquiredAt:    now.Format(time.RFC3339),
			ExpiresAt:     expiresAt.Format(time.RFC3339),
			LastHeartbeat: now.Format(time.RFC3339),
		}
		s.store.EnsureDir("locks", "leases")
		return s.store.WriteJSON(s.store.Path("locks", "leases", leaseID+".json"), lease)
	})

	if err != nil {
		return nil, err
	}

	// Read back the lease
	var lease Lease
	if err := s.store.ReadJSON(s.store.Path("locks", "leases", leaseID+".json"), &lease); err != nil {
		// Read from the files instead
		dir := s.store.Path("locks", "leases")
		leaseFiles, _ := s.store.ListJSONFiles(dir)
		for _, lf := range leaseFiles {
			var l Lease
			if err := s.store.ReadJSON(lf, &l); err == nil && l.Owner == owner {
				if len(l.Files) == len(files) {
					lease = l
					break
				}
			}
		}
	}

	// Find the lease we just created by scanning
	if lease.LeaseID == "" {
		// Fallback: scan lease dir for the one we created
		dir := s.store.Path("locks", "leases")
		leaseFiles, _ := s.store.ListJSONFiles(dir)
		for i := len(leaseFiles) - 1; i >= 0; i-- {
			var l Lease
			if err := s.store.ReadJSON(leaseFiles[i], &l); err == nil && l.Owner == owner {
				lease = l
				break
			}
		}
	}

	s.trace.Log(TraceEvent{
		Type:    EventLockAcquired,
		Actor:   owner,
		Subject: lease.LeaseID,
		Detail:  fmt.Sprintf("files: %v, ttl: %ds", files, ttlSec),
	})

	return &lease, nil
}

// Heartbeat extends the TTL of a lease.
func (s *LockService) Heartbeat(leaseID string, extendSec int) (*Lease, error) {
	if leaseID == "" {
		return nil, fmt.Errorf("lease_id is required")
	}
	if extendSec <= 0 {
		extendSec = 120
	}

	var result *Lease
	err := s.store.WithLock(func() error {
		leasePath := s.store.Path("locks", "leases", leaseID+".json")
		var lease Lease
		if err := s.store.ReadJSON(leasePath, &lease); err != nil {
			return fmt.Errorf("lease '%s' not found", leaseID)
		}

		now := time.Now().UTC()
		newExpires := now.Add(time.Duration(extendSec) * time.Second)
		lease.ExpiresAt = newExpires.Format(time.RFC3339)
		lease.LastHeartbeat = now.Format(time.RFC3339)

		// Update lease file
		if err := s.store.WriteJSON(leasePath, &lease); err != nil {
			return err
		}

		// Update individual file locks
		for _, file := range lease.Files {
			hash := PathHash(file)
			lockPath := s.store.Path("locks", "files", hash+".json")
			var fl FileLock
			if err := s.store.ReadJSON(lockPath, &fl); err == nil && fl.LeaseID == leaseID {
				fl.ExpiresAt = lease.ExpiresAt
				fl.LastHeartbeat = lease.LastHeartbeat
				_ = s.store.WriteJSON(lockPath, &fl)
			}
		}

		result = &lease
		return nil
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventLockHeartbeat,
			Actor:   result.Owner,
			Subject: leaseID,
		})
	}

	return result, err
}

// Unlock releases a lease and all its file locks.
func (s *LockService) Unlock(leaseID string) error {
	if leaseID == "" {
		return fmt.Errorf("lease_id is required")
	}

	var lease Lease
	err := s.store.WithLock(func() error {
		leasePath := s.store.Path("locks", "leases", leaseID+".json")
		if err := s.store.ReadJSON(leasePath, &lease); err != nil {
			return fmt.Errorf("lease '%s' not found", leaseID)
		}

		// Remove file locks
		for _, file := range lease.Files {
			hash := PathHash(file)
			lockPath := s.store.Path("locks", "files", hash+".json")
			var fl FileLock
			if err := s.store.ReadJSON(lockPath, &fl); err == nil && fl.LeaseID == leaseID {
				_ = s.store.Remove(lockPath)
			}
		}

		// Remove lease
		return s.store.Remove(leasePath)
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventLockReleased,
			Actor:   lease.Owner,
			Subject: leaseID,
			Detail:  fmt.Sprintf("files: %v", lease.Files),
		})
	}

	return err
}

// ForceUnlock forcefully removes a lease (Leader only).
func (s *LockService) ForceUnlock(leaseID, reason string) error {
	if leaseID == "" {
		return fmt.Errorf("lease_id is required")
	}

	var lease Lease
	err := s.store.WithLock(func() error {
		leasePath := s.store.Path("locks", "leases", leaseID+".json")
		if err := s.store.ReadJSON(leasePath, &lease); err != nil {
			return fmt.Errorf("lease '%s' not found", leaseID)
		}

		for _, file := range lease.Files {
			hash := PathHash(file)
			lockPath := s.store.Path("locks", "files", hash+".json")
			_ = s.store.Remove(lockPath)
		}

		return s.store.Remove(leasePath)
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventLockForced,
			Actor:   "leader",
			Subject: leaseID,
			Detail:  fmt.Sprintf("reason: %s, owner was: %s, files: %v", reason, lease.Owner, lease.Files),
		})
	}

	return err
}

// ListLocks returns all active locks, optionally filtered.
func (s *LockService) ListLocks(owner string, files []string) ([]Lease, error) {
	dir := s.store.Path("locks", "leases")
	leaseFiles, err := s.store.ListJSONFiles(dir)
	if err != nil {
		return []Lease{}, nil
	}

	now := time.Now().UTC()
	var result []Lease

	for _, lf := range leaseFiles {
		var lease Lease
		if err := s.store.ReadJSON(lf, &lease); err != nil {
			continue
		}

		// Skip expired
		expTime, _ := time.Parse(time.RFC3339, lease.ExpiresAt)
		if now.After(expTime) {
			continue
		}

		// Filter by owner
		if owner != "" && lease.Owner != owner {
			continue
		}

		// Filter by files
		if len(files) > 0 {
			match := false
			for _, f := range files {
				for _, lf := range lease.Files {
					if filepath.Clean(f) == filepath.Clean(lf) {
						match = true
						break
					}
				}
				if match {
					break
				}
			}
			if !match {
				continue
			}
		}

		result = append(result, lease)
	}

	return result, nil
}

// CleanExpired removes expired locks (called periodically or on demand).
func (s *LockService) CleanExpired() (int, error) {
	cleaned := 0

	err := s.store.WithLock(func() error {
		now := time.Now().UTC()

		// Clean expired leases
		dir := s.store.Path("locks", "leases")
		leaseFiles, _ := s.store.ListJSONFiles(dir)

		for _, lf := range leaseFiles {
			var lease Lease
			if err := s.store.ReadJSON(lf, &lease); err != nil {
				continue
			}

			expTime, _ := time.Parse(time.RFC3339, lease.ExpiresAt)
			if now.After(expTime) {
				// Remove file locks
				for _, file := range lease.Files {
					hash := PathHash(file)
					lockPath := s.store.Path("locks", "files", hash+".json")
					_ = s.store.Remove(lockPath)
				}
				_ = s.store.Remove(lf)
				cleaned++
			}
		}

		// Also clean orphaned file locks
		fileDir := s.store.Path("locks", "files")
		fileLocks, _ := s.store.ListJSONFiles(fileDir)
		for _, fl := range fileLocks {
			var lock FileLock
			if err := s.store.ReadJSON(fl, &lock); err != nil {
				continue
			}
			expTime, _ := time.Parse(time.RFC3339, lock.ExpiresAt)
			if now.After(expTime) {
				_ = s.store.Remove(fl)
				cleaned++
			}
		}

		return nil
	})

	return cleaned, err
}

func init() {
	// Ensure locks directories exist at package init
	_ = os.MkdirAll(filepath.Join(os.TempDir(), "swarm-mcp-locks"), 0755)
}
