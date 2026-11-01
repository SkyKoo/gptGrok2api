package httpapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenancePausesRetentionAndRemovalResumesOriginalPolicy(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	cfg.ImageRetentionDays = 1
	s := &Server{cfg: cfg, imageTasks: map[string]*imageTaskState{}}
	old := time.Now().Add(-25 * time.Hour)
	var expired []string
	for _, root := range []string{cfg.ImageDataDir, s.logInputImagesDir()} {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "expired.png")
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		expired = append(expired, path)
	}
	fresh := filepath.Join(cfg.ImageDataDir, "fresh.png")
	if err := os.WriteFile(fresh, []byte("fresh"), 0600); err != nil {
		t.Fatal(err)
	}
	task := &imageTaskState{ID: "completed", OwnerID: "test-owner", Status: "success", UpdatedAt: old.UTC().Format(time.RFC3339)}
	s.imageTasks[imageTaskKey(task.OwnerID, task.ID)] = task
	if err := s.saveImageTaskLocked(task); err != nil {
		t.Fatal(err)
	}
	taskFile := filepath.Join(s.imageTasksDir(), imageTaskKey(task.OwnerID, task.ID)+".json")
	marker := filepath.Join(cfg.DataDir, "cfm-maintenance")
	if err := os.WriteFile(marker, []byte("deploy:test"), 0600); err != nil {
		t.Fatal(err)
	}
	if s.cleanupImagesOutsideMaintenance() {
		t.Fatal("cleanup ran during maintenance")
	}
	for _, path := range append(expired, taskFile) {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("maintenance changed %s: %v", path, err)
		}
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if !s.cleanupImagesOutsideMaintenance() {
		t.Fatal("cleanup did not resume")
	}
	for _, path := range append(expired, taskFile) {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expired file retained: %s %v", path, err)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh file was removed")
	}
}
