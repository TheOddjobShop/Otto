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
