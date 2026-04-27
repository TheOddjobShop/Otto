package telegram

import "context"

// MaxMessageLen is Telegram's per-message limit.
const MaxMessageLen = 4096

// SendChunked sends `text` to chatID, splitting into multiple messages if it
// exceeds MaxMessageLen. Empty text is a no-op.
func SendChunked(ctx context.Context, c BotClient, chatID int64, text string) error {
	for _, chunk := range ChunkMessage(text, MaxMessageLen) {
		if err := c.SendMessage(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}
