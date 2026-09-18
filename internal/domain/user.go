package domain

import "time"

type User struct {
	TelegramID   int64
	Username     string
	FirstName    string
	LastName     string
	LanguageCode string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (u User) DisplayName() string {
	if u.Username != "" {
		return "@" + u.Username
	}
	if u.FirstName == "" {
		return u.LastName
	}
	if u.LastName == "" {
		return u.FirstName
	}
	return u.FirstName + " " + u.LastName
}
