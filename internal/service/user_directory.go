package service

import (
	"context"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// UserLookup resolves one user account by id.
type UserLookup interface {
	LoadUserByID(ctx context.Context, id string) (*storageif.User, error)
}

// UserDirectory answers questions about other user accounts that org routes need without
// owning the account: how to name a member, whether an invitee's email is verified.
type UserDirectory struct {
	users UserLookup
}

func NewUserDirectory(users UserLookup) *UserDirectory {
	return &UserDirectory{users: users}
}

// Load returns the user with id; a missing user is the store's error.
func (d *UserDirectory) Load(ctx context.Context, id string) (*storageif.User, error) {
	return d.users.LoadUserByID(ctx, id)
}
