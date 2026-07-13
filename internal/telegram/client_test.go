package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestRealClientSendMessageHitsAPI(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 99}, "date": 0},
		})
	}))
	defer srv.Close()

	c, err := NewBotClient("test-token", srv.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("NewBotClient: %v", err)
	}

	if err := c.SendMessage(context.Background(), 99, "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !strings.Contains(gotPath, "sendMessage") {
		t.Errorf("path = %q, want contains sendMessage", gotPath)
	}
	if !strings.Contains(gotBody, "hi") {
		t.Errorf("body = %q, want contains 'hi'", gotBody)
	}
	if !strings.Contains(gotBody, "chat_id=99") {
		t.Errorf("body = %q, want contains 'chat_id=99'", gotBody)
	}
}

func TestRedactTokenStripsURLError(t *testing.T) {
	const token = "123456:AAE-super-secret"
	ue := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + token + "/getUpdates",
		Err: errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
	}
	got := redactToken(ue, token)
	if strings.Contains(got.Error(), token) {
		t.Errorf("token leaked: %q", got)
	}
	if !strings.Contains(got.Error(), "<redacted>") {
		t.Errorf("missing redaction marker: %q", got)
	}
	if !strings.Contains(got.Error(), "connection reset by peer") {
		t.Errorf("underlying cause lost: %q", got)
	}
	if redactToken(nil, token) != nil {
		t.Error("redactToken(nil) should stay nil")
	}
}

func TestStripURLErrorDropsTokenBearingURL(t *testing.T) {
	const token = "123456:AAE-super-secret"
	cause := errors.New("context deadline exceeded")
	ue := &url.Error{
		Op:  "Get",
		URL: "https://api.telegram.org/file/bot" + token + "/photos/file_1.jpg",
		Err: cause,
	}
	if got := stripURLError(ue); got != cause {
		t.Errorf("got %v, want unwrapped cause", got)
	}
	plain := errors.New("plain")
	if got := stripURLError(plain); got != plain {
		t.Errorf("non-url.Error should pass through, got %v", got)
	}
}

func TestGetUpdatesRedactsTokenOnTransportError(t *testing.T) {
	const token = "123456:AAE-super-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getMe") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "otto"},
			})
			return
		}
		// Kill the connection mid-request so the client surfaces a
		// *url.Error whose message embeds the token-bearing request URL.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	c, err := NewBotClient(token, srv.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("NewBotClient: %v", err)
	}
	_, err = c.GetUpdates(context.Background(), 0)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("bot token leaked in error: %q", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("missing redaction marker in error: %q", err)
	}
}

func TestFromTGUpdate(t *testing.T) {
	tests := []struct {
		name string
		in   tgbotapi.Update
		want Update
	}{
		{
			name: "no message",
			in:   tgbotapi.Update{UpdateID: 7},
			want: Update{UpdateID: 7},
		},
		{
			name: "no user (channel post)",
			in: tgbotapi.Update{
				UpdateID: 8,
				Message:  &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}, Text: "hi"},
			},
			want: Update{UpdateID: 8, ChatID: 100, Text: "hi"},
		},
		{
			name: "caption fallback",
			in: tgbotapi.Update{
				UpdateID: 9,
				Message: &tgbotapi.Message{
					Chat:    &tgbotapi.Chat{ID: 100},
					From:    &tgbotapi.User{ID: 99},
					Caption: "a caption",
				},
			},
			want: Update{UpdateID: 9, ChatID: 100, UserID: 99, Text: "a caption"},
		},
		{
			name: "text wins over caption",
			in: tgbotapi.Update{
				UpdateID: 10,
				Message: &tgbotapi.Message{
					Chat:    &tgbotapi.Chat{ID: 100},
					From:    &tgbotapi.User{ID: 99},
					Text:    "primary",
					Caption: "secondary",
				},
			},
			want: Update{UpdateID: 10, ChatID: 100, UserID: 99, Text: "primary"},
		},
		{
			name: "photo sizes - largest wins",
			in: tgbotapi.Update{
				UpdateID: 11,
				Message: &tgbotapi.Message{
					Chat: &tgbotapi.Chat{ID: 100},
					From: &tgbotapi.User{ID: 99},
					Photo: []tgbotapi.PhotoSize{
						{FileID: "small"},
						{FileID: "medium"},
						{FileID: "large"},
					},
				},
			},
			want: Update{UpdateID: 11, ChatID: 100, UserID: 99, PhotoIDs: []string{"large"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fromTGUpdate(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fromTGUpdate = %+v, want %+v", got, tc.want)
			}
		})
	}
}
