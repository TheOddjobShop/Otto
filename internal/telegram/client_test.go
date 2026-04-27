package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
