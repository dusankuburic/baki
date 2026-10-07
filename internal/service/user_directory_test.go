package service

import (
	"context"
	"errors"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

type userLookupStub map[string]*storageif.User

func (s userLookupStub) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	if u, ok := s[id]; ok {
		return u, nil
	}
	return nil, storageif.ErrNotFound
}

func TestUserDirectory_Load(t *testing.T) {
	d := NewUserDirectory(userLookupStub{"u-1": {ID: "u-1", Email: "a@example.com", EmailVerified: true}})
	if u, err := d.Load(context.Background(), "u-1"); err != nil || !u.EmailVerified {
		t.Fatalf("Load(u-1) = %+v, %v", u, err)
	}
	if _, err := d.Load(context.Background(), "u-2"); !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("Load(u-2) error = %v, want ErrNotFound", err)
	}
}
