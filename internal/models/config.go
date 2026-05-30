package models

// LogConfig controls logger initialisation. In cloud (containerized)
// deployments leave Level empty for info; the orchestrator handles
// log collection and rotation off the pod's stdout.
type LogConfig struct {
	// Level: "debug", "info", "warn", "error" (default: info).
	Level string
}
