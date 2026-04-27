package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func extensionFor(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".bin"
}

// DownloadPhotoToTemp downloads the file by ID and writes it to a uniquely
// named file under dir, returning the absolute path. Caller is responsible
// for cleanup.
func DownloadPhotoToTemp(ctx context.Context, c BotClient, fileID, dir string) (string, error) {
	data, contentType, err := c.DownloadFile(ctx, fileID)
	if err != nil {
		return "", fmt.Errorf("telegram: download %s: %w", fileID, err)
	}
	ext := extensionFor(contentType)
	f, err := os.CreateTemp(dir, "tgphoto-*"+ext)
	if err != nil {
		return "", fmt.Errorf("telegram: create temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("telegram: write %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("telegram: close %s: %w", f.Name(), err)
	}
	abs, err := filepath.Abs(f.Name())
	if err != nil {
		return "", fmt.Errorf("telegram: abs path: %w", err)
	}
	return abs, nil
}
