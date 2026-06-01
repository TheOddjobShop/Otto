// Package auth gates incoming Telegram messages by user ID.
package auth

type Allowlist struct {
	allowed int64
}

func New(allowedUserID int64) *Allowlist {
	return &Allowlist{allowed: allowedUserID}
}

func (a *Allowlist) Allows(userID int64) bool {
	return userID == a.allowed
}

// UserID returns the single allowed Telegram user id. Used by the bus
// drain (and other out-of-band senders) which need a chat id to address
// the user without an incoming Update to copy it from.
func (a *Allowlist) UserID() int64 {
	return a.allowed
}
