package storage

import (
	"os"
	"path/filepath"
	"time"

	"pad-analyzer/internal/models"
)

const maxRecentFiles = 10

func AddRecentFile(s *SettingsStore, path string, size int64) error {
	settings := s.Get()
	name := filepath.Base(path)
	now := time.Now()

	isFolder := false
	if info, err := os.Stat(path); err == nil {
		isFolder = info.IsDir()
	}

	filtered := make([]models.RecentFile, 0, len(settings.RecentFiles))
	for _, f := range settings.RecentFiles {
		if f.Path != path {
			filtered = append(filtered, f)
		}
	}

	entry := models.RecentFile{Path: path, Name: name, Size: size, LastOpen: now, IsFolder: isFolder}
	updated := make([]models.RecentFile, 0, maxRecentFiles)
	updated = append(updated, entry)
	updated = append(updated, filtered...)
	if len(updated) > maxRecentFiles {
		updated = updated[:maxRecentFiles]
	}

	clone := *settings
	clone.RecentFiles = updated
	return s.Update(clone)
}

func RemoveRecentFile(s *SettingsStore, path string) error {
	settings := s.Get()
	filtered := make([]models.RecentFile, 0, len(settings.RecentFiles))
	for _, f := range settings.RecentFiles {
		if f.Path != path {
			filtered = append(filtered, f)
		}
	}
	clone := *settings
	clone.RecentFiles = filtered
	return s.Update(clone)
}

func ClearRecentFiles(s *SettingsStore) error {
	settings := s.Get()
	clone := *settings
	clone.RecentFiles = nil
	return s.Update(clone)
}

func PurgeMissingRecentFiles(s *SettingsStore) error {
	settings := s.Get()
	filtered := make([]models.RecentFile, 0, len(settings.RecentFiles))
	for _, f := range settings.RecentFiles {
		if _, err := os.Stat(f.Path); err == nil {
			filtered = append(filtered, f)
		}
	}
	clone := *settings
	clone.RecentFiles = filtered
	return s.Update(clone)
}
