package telegram

import (
	"context"
	"strings"
	"testing"
)

type fakeClient struct {
	sent []string
}

func (f *fakeClient) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	return nil, nil
}
func (f *fakeClient) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeClient) SendMessageHTML(ctx context.Context, chatID int64, text string) error {
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeClient) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return nil, "", nil
}
func (f *fakeClient) SendMessageWithButtons(ctx context.Context, chatID int64, text string, buttons [][]InlineButton) error {
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeClient) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
	return nil
}

func TestSendChunkedShortMessage(t *testing.T) {
	f := &fakeClient{}
	if err := SendChunked(context.Background(), f, 1, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "hello" {
		t.Errorf("sent = %v", f.sent)
	}
}

func TestSendChunkedLongMessage(t *testing.T) {
	f := &fakeClient{}
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	if err := SendChunked(context.Background(), f, 1, a+"\n\n"+b); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 2 {
		t.Fatalf("got %d sends, want 2", len(f.sent))
	}
}

func TestSendChunkedEmpty(t *testing.T) {
	f := &fakeClient{}
	if err := SendChunked(context.Background(), f, 1, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 {
		t.Errorf("sent = %v, want []", f.sent)
	}
}
