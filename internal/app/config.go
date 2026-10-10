package app

import (
	"pad-analyzer/internal/config"
)

// loadConfig provides the configuration config.Load builds. It returns an error instead of exiting on a fatal
// misconfiguration, so fx handles "can't start" through its provider-error path.
func loadConfig() (*config.Config, error) {
	return config.Load()
}
