package storage

import (
	"fmt"

	"github.com/zalando/go-keyring"
)

const keyringService = "pad-analyzer"

func SaveApiKey(provider string, key string) error {
	return keyring.Set(keyringService, "apikey:"+provider, key)
}

func GetApiKey(provider string) (string, error) {
	return keyring.Get(keyringService, "apikey:"+provider)
}

func HasApiKey(provider string) (bool, error) {
	_, err := keyring.Get(keyringService, "apikey:"+provider)
	if err == keyring.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking keychain for %s: %w", provider, err)
	}
	return true, nil
}

func DeleteApiKey(provider string) error {
	return keyring.Delete(keyringService, "apikey:"+provider)
}
