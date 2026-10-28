package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/events"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/storage/settingsdoc"
	"pad-core/logger"
	"pad-core/models"
)

// The section-level org settings write (R-config-4, the follow-up ADR 0012 named). A whole-document write reads the
// stored document outside the save's lock and replaces each section it names whole, so two admins saving different
// fields of ai at once lose one change, the fail-closed allowed-provider list included. A section write instead names
// fields of one section, and the storage merges them into the section as stored under the settings rows' lock
// (UpdateOrgSettingsWith): concurrent writes to different fields both land. A field-level merge alone cannot tell a
// writer that the field it changes moved since it read it, so a write may carry the revision it read; a stale one is
// refused with ErrSettingsConflict (409), and the client reloads before retrying.

// ErrSettingsConflict refuses a section write whose revision the stored section has moved past. The API maps it to
// 409 with SettingsCodeStale.
var ErrSettingsConflict = errors.New("the settings changed since they were read")

// SettingsCodeStale is the machine code of a refused stale section write.
const SettingsCodeStale = "SETTINGS_STALE"

// SettingsSectionPatch is a section write: the fields of Section it sets, each a JSON value (null clears the field),
// and the revision the writer read, "" for none.
type SettingsSectionPatch struct {
	Section    string
	Values     map[string]json.RawMessage
	IfRevision string
}

// SettingsSectionState is a section as stored and its revision, the opaque token a write's precondition carries:
// it changes whenever the section's content does.
type SettingsSectionState struct {
	Section  string          `json:"section"`
	Values   json.RawMessage `json:"values" swaggertype:"object"`
	Revision string          `json:"revision"`
	// Fields are the section's own names of the fields a write set, sorted (a body may name them in any case);
	// empty on a read. Not on the wire: the route's audit row carries them.
	Fields []string `json:"-"`
}

// settingsSection finds the document field of a section: one of the document's top-level keys, except the document
// version and the server-owned recent files, which no settings write sets.
func settingsSection(section string) (int, error) {
	t := reflect.TypeFor[models.AppSettings]()
	for i := range t.NumField() {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if key == section && key != settingsdoc.DocVersionKey && key != serverOwnedSettingsKey {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%w: %q is not a settings section a write can set", ErrSettingsInvalid, section)
}

// sectionState is the section of settings at field index i, with its revision: a digest of the section as the
// document carries it, so a read and a write's precondition check compute it alike.
func sectionState(settings *models.AppSettings, section string, i int) (SettingsSectionState, error) {
	raw, err := json.Marshal(reflect.ValueOf(settings).Elem().Field(i).Interface())
	if err != nil {
		return SettingsSectionState{}, fmt.Errorf("encode settings section %s: %w", section, err)
	}
	sum := sha256.Sum256(append([]byte(section+"\x00"), raw...))
	return SettingsSectionState{Section: section, Values: raw, Revision: hex.EncodeToString(sum[:16])}, nil
}

// GetOrgSettingsSection returns one section of the organisation's settings with its revision.
func (s *SystemService) GetOrgSettingsSection(ctx context.Context, orgID, section string) (state SettingsSectionState, err error) {
	defer logger.Guard("SystemService.GetOrgSettingsSection", &err)
	i, err := settingsSection(section)
	if err != nil {
		return SettingsSectionState{}, err
	}
	settings, err := s.GetOrgSettings(ctx, orgID)
	if err != nil {
		return SettingsSectionState{}, err
	}
	return sectionState(settings, section, i)
}

// PatchOrgSettingsSection merges the fields p names into the section as stored, under the settings rows' lock, and
// answers with the section as written and its new revision. A stale p.IfRevision is ErrSettingsConflict; a section
// or field no write can set, a value of the wrong type, or one the section's rules refuse is ErrSettingsInvalid (an
// unknown id in ai.allowedProviders is *UnknownProvidersError, as on the whole-document write); nothing is written
// then. Without a server store the deployment's document is the organisation's, as on UpdateOrgSettings.
func (s *SystemService) PatchOrgSettingsSection(ctx context.Context, orgID string, p SettingsSectionPatch) (out SettingsSectionState, err error) {
	defer logger.Guard("SystemService.PatchOrgSettingsSection", &err)
	i, err := settingsSection(p.Section)
	if err != nil {
		return SettingsSectionState{}, err
	}
	values, err := canonicalSectionFields(p.Section, i, p.Values)
	if err != nil {
		return SettingsSectionState{}, err
	}
	var written *models.AppSettings
	apply := func(doc *models.AppSettings) error {
		cur, err := sectionState(doc, p.Section, i)
		if err != nil {
			return err
		}
		if p.IfRevision != "" && p.IfRevision != cur.Revision {
			return fmt.Errorf("%w: %s is at another revision now; reload it and apply the change again", ErrSettingsConflict, p.Section)
		}
		next := *doc
		if err := mergeSectionFields(&next, p.Section, i, cur.Values, values); err != nil {
			return err
		}
		if err := checkSectionWrite(&next, doc, p.Section, values); err != nil {
			return err
		}
		*doc = next
		written = doc.Clone()
		return nil
	}
	if s.backend == nil {
		if s.settings == nil {
			return SettingsSectionState{}, fmt.Errorf("settings store not initialized")
		}
		if err := updateSettingsWith(ctx, s.settings, apply); err != nil {
			return SettingsSectionState{}, err
		}
		s.bus.Publish(ctx, events.SettingsChanged{Scope: events.DeploymentSettings})
		s.notifier.EmitTo(LocalUser, "settings:changed", *written)
		return writtenSectionState(written, p.Section, i, values)
	}
	err = s.backend.UpdateOrgSettingsWith(ctx, orgID, func(stored *storageif.AppSettings) error {
		doc := settingsToModel(stored)
		if err := apply(doc); err != nil {
			return err
		}
		next := settingsFromModel(doc)
		if next == nil {
			return fmt.Errorf("encode org settings")
		}
		*stored = *next
		return nil
	})
	if err != nil {
		return SettingsSectionState{}, err
	}
	// The org's settings are its rule profile (Analysis.Rules) and its provider policy (AI.AllowedProviders).
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: orgID})
	s.notifier.EmitToOrg(ctx, orgID, "settings:changed", *written)
	return writtenSectionState(written, p.Section, i, values)
}

// writtenSectionState is the section as a write left it, with the canonical names of the fields it set.
func writtenSectionState(written *models.AppSettings, section string, i int, values map[string]json.RawMessage) (SettingsSectionState, error) {
	state, err := sectionState(written, section, i)
	if err != nil {
		return SettingsSectionState{}, err
	}
	for k := range values {
		state.Fields = append(state.Fields, k)
	}
	slices.Sort(state.Fields)
	return state, nil
}

// canonicalSectionFields maps the fields a write names to the section's own keys (matched regardless of case, as
// encoding/json matches them), refusing a write that names none, a key the section does not have, or one twice.
func canonicalSectionFields(section string, i int, values map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: a write to %s names no field", ErrSettingsInvalid, section)
	}
	keys := settingsdoc.DocumentKeys(reflect.Zero(reflect.TypeFor[models.AppSettings]().Field(i).Type).Interface())
	out := make(map[string]json.RawMessage, len(values))
	for k, v := range values {
		at := slices.IndexFunc(keys, func(key string) bool { return strings.EqualFold(key, k) })
		if at < 0 {
			return nil, fmt.Errorf("%w: %s.%s is not a field of the section", ErrSettingsInvalid, section, k)
		}
		if _, dup := out[keys[at]]; dup {
			return nil, fmt.Errorf("%w: %s.%s is named twice", ErrSettingsInvalid, section, keys[at])
		}
		if len(bytes.TrimSpace(v)) == 0 {
			v = json.RawMessage("null")
		}
		out[keys[at]] = v
	}
	return out, nil
}

// mergeSectionFields sets the named fields of doc's section (stored is its current JSON) and keeps the rest: the
// merged object is decoded into a fresh value, so an explicit null clears a field.
func mergeSectionFields(doc *models.AppSettings, section string, i int, stored json.RawMessage, values map[string]json.RawMessage) error {
	merged := map[string]json.RawMessage{}
	if err := json.Unmarshal(stored, &merged); err != nil {
		return fmt.Errorf("decode settings section %s: %w", section, err)
	}
	for k, v := range values {
		merged[k] = v
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encode settings section %s: %w", section, err)
	}
	field := reflect.ValueOf(doc).Elem().Field(i)
	fresh := reflect.New(field.Type())
	if err := json.Unmarshal(raw, fresh.Interface()); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrSettingsInvalid, section, err)
	}
	field.Set(fresh.Elem())
	return nil
}

// checkSectionWrite applies the rules of the whole-document write to the section the write changed: the profile's
// caps; the ai section's caps against the stored values (validateAISection: a value carried back unchanged is never
// refused), and a named allowed-provider list must name registered providers only (it is fail-closed, so a typo
// would lock the organisation out of every provider).
func checkSectionWrite(next, stored *models.AppSettings, section string, values map[string]json.RawMessage) error {
	switch section {
	case "profile":
		return validateProfileSection(next.Profile)
	case "ai":
		if _, named := values["allowedProviders"]; named {
			var unknown []string
			for _, id := range next.AI.AllowedProviders {
				if !ai.KnownProvider(id) {
					unknown = append(unknown, id)
				}
			}
			if len(unknown) > 0 {
				return &UnknownProvidersError{IDs: unknown}
			}
		}
		return validateAISection(next.AI, stored.AI)
	}
	return nil
}
