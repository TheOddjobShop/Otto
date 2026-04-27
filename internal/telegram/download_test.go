package telegram

import (
	"context"
	"os"
	"strings"
	"testing"
)

type fakeDownloader struct {
	data        []byte
	contentType string
}

func (f *fakeDownloader) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	return nil, nil
}
func (f *fakeDownloader) SendMessage(ctx context.Context, chatID int64, text string) error {
	return nil
}
func (f *fakeDownloader) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return f.data, f.contentType, nil
}
func (f *fakeDownloader) SendMessageWithButtons(ctx context.Context, chatID int64, text string, buttons [][]InlineButton) error {
	return nil
}
func (f *fakeDownloader) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
	return nil
}

func TestDownloadPhotoToTemp(t *testing.T) {
	f := &fakeDownloader{data: []byte("\x89PNG\x0D\x0A\x1A\x0A"), contentType: "image/png"}
	dir := t.TempDir()

	path, err := DownloadPhotoToTemp(context.Background(), f, "FILE-ABC", dir)
	if err != nil {
		t.Fatalf("DownloadPhotoToTemp: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("path %q not under tempdir %q", path, dir)
	}
	if !strings.HasSuffix(path, ".png") {
		t.Errorf("path %q missing .png extension", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(f.data) {
		t.Errorf("contents = %q, want %q", got, f.data)
	}
}

func TestDownloadPhotoUnknownContentType(t *testing.T) {
	f := &fakeDownloader{data: []byte("data"), contentType: ""}
	dir := t.TempDir()

	path, err := DownloadPhotoToTemp(context.Background(), f, "FILE-ABC", dir)
	if err != nil {
		t.Fatalf("DownloadPhotoToTemp: %v", err)
	}
	if !strings.HasSuffix(path, ".bin") {
		t.Errorf("path %q missing .bin fallback extension", path)
	}
}
