package store

import (
	"strings"

	"github.com/google/uuid"
)

// NewID returns a UUIDv7 string: time-ordered, so default index order is creation order.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

// ShortID is the first 8 characters, used in branch names and chat replies.
func ShortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
