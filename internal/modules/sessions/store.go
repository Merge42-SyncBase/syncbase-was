// Package sessions defines persistence for authenticated browser sessions.
package sessions

import (
	"context"
	"time"
)

// Record contains server-side session state that is safe to return to the
// authenticated browser. Implementations must not persist the raw bearer token.
type Record struct {
	CSRFToken string
	ExpiresAt time.Time
}

// Store persists browser sessions across web process restarts.
//
// Tokens are opaque bearer credentials supplied by an untrusted cookie. Store
// implementations must expire a record at or before its ExpiresAt time and
// must remove it on Delete.
type Store interface {
	Create(context.Context, string, Record) error
	Load(context.Context, string, time.Time) (Record, bool, error)
	Delete(context.Context, string) error
}
