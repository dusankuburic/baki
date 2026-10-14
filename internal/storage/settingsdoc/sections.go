package settingsdoc

// Default is the sections of the settings document, one per top-level key of AppSettings (core/models/settings.go,
// mirrored by storage/interfaces). Each entry names the code that owns the section's fields.
//
// When a section's stored shape changes in a way decoding cannot absorb (a renamed field, a changed meaning, a
// value that moves to another field), its owner bumps Version and adds the upgrade from the previous version
// here, in the entry or in a file of its own once there are several. A field added with a usable zero value needs
// no new version. Never give an existing field another JSON type: a binary one release older decodes the section
// field by field and keeps a value it cannot decode, but cannot edit it. While the legacy blob is still written
// (ADR 0012), it carries every section at LegacyVersion, so a version bump must wait for the release that stops
// writing it, or write the blob in the old shape.
var Default = MustRegistry(
	// The user's identity card: bio, job title, timezone. The profile page reads it through /api/auth/me.
	Section{Key: "profile", Version: 1},
	// The app shell: whether the first-run walkthrough was completed.
	Section{Key: "general", Version: 1},
	// The frontend's theme, density, motion and chat text size.
	Section{Key: "appearance", Version: 1},
	// The frontend's panel sizes and last view.
	Section{Key: "layout", Version: 1},
	// internal/ai and the chat: providers, models, budgets, prompts.
	Section{Key: "ai", Version: 1},
	// The flow parser's limits.
	Section{Key: "parser", Version: 1},
	// The analyzer's rule configuration.
	Section{Key: "analysis", Version: 1},
	// The desktop's recently opened files (internal/storage/recent.go); empty in cloud mode.
	Section{Key: "recentFiles", Version: 1},
)
