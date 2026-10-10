package httpapi

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// imageRetentionScheduler keeps generated images and their metadata from
// growing without bound. A file is only eligible after the full retention
// period has elapsed, so newly generated files are never removed mid-request.
func (s *Server) imageRetentionScheduler() {
	cleanup := func() {
		s.cleanupExpiredImageTasks()
		removed, bytes := s.cleanupExpiredImages()
		if removed > 0 {
			log.Printf("image retention cleanup removed %d files (%d bytes)", removed, bytes)
		}
	}
	cleanup()
	interval := s.cfg.ImageCleanupInterval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		cleanup()
	}
}

func (s *Server) cleanupExpiredImages() (int, int64) {
	days := s.cfg.ImageRetentionDays
	if days < 1 {
		days = 1
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	removed := 0
	var removedBytes int64
	for _, root := range []string{s.cfg.ImageDataDir, s.logInputImagesDir()} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || !info.ModTime().Before(cutoff) {
				return nil
			}
			if removeErr := os.Remove(path); removeErr == nil {
				removed++
				removedBytes += info.Size()
			}
			return nil
		})
		cleanupEmptyDirs(root)
	}
	return removed, removedBytes
}
