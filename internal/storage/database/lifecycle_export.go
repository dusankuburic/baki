package database

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// exportSection is one top-level key of the data-subject export bundle. The buffered path (ExportUserData) fills
// the interfaces.UserDataExport field, the streamed path (ExportUserDataTo) writes the key and its value; both run
// the sections in the order of the struct's fields, so the two cannot drift apart.
type exportSection struct {
	// key is the section's json key in interfaces.UserDataExport.
	key string
	// typ is the section's Go type, which must be its UserDataExport field's.
	typ  reflect.Type
	fill func(ctx context.Context, b *PostgresStorageBackend, x *exportRun, out *interfaces.UserDataExport) error
	// write writes the section's value as JSON.
	write func(ctx context.Context, b *PostgresStorageBackend, x *exportRun, w *bufio.Writer) error
}

// exportRun is the state of one export.
type exportRun struct {
	userID string
	// flowIDs are the flows the user owns, collected by the flows section: the conversation and triage sections,
	// which come after it in the bundle, are scoped by them.
	flowIDs []string
}

// section declares a bundle section that load reads and set stores in the buffered bundle; the streamed path
// marshals load's value.
func section[T any](key string, load func(context.Context, *PostgresStorageBackend, *exportRun) (T, error), set func(*interfaces.UserDataExport, T)) exportSection {
	return exportSection{
		key: key,
		typ: reflect.TypeFor[T](),
		fill: func(ctx context.Context, b *PostgresStorageBackend, x *exportRun, out *interfaces.UserDataExport) error {
			v, err := load(ctx, b, x)
			if err != nil {
				return err
			}
			set(out, v)
			return nil
		},
		write: func(ctx context.Context, b *PostgresStorageBackend, x *exportRun, w *bufio.Writer) error {
			v, err := load(ctx, b, x)
			if err != nil {
				return err
			}
			return writeJSON(w, v)
		},
	}
}

// writeJSON writes v's JSON encoding to w, as json.Marshal (and so the buffered bundle) renders it.
func writeJSON(w *bufio.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// exportKeyOrder maps each json key of interfaces.UserDataExport to its field's position.
var exportKeyOrder = sync.OnceValue(func() map[string]int {
	t := reflect.TypeFor[interfaces.UserDataExport]()
	order := make(map[string]int, t.NumField())
	for i := range t.NumField() {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		order[key] = i
	}
	return order
})

// exportSections returns every domain's sections in the bundle's key order.
func exportSections() []exportSection {
	var out []exportSection
	for _, d := range lifecycleDomains {
		out = append(out, d.export...)
	}
	order := exportKeyOrder()
	// A key the struct lacks sorts last; TestLifecycle_ExportSectionsAreTheBundle refuses it.
	pos := func(key string) int {
		if i, ok := order[key]; ok {
			return i
		}
		return len(order)
	}
	slices.SortStableFunc(out, func(a, b exportSection) int { return cmp.Compare(pos(a.key), pos(b.key)) })
	return out
}

// ExportUserData assembles the user's personal data for a data-subject access/portability request from every
// domain's export sections (lifecycle_*.go): the account, the flows the user owns, settings, audit history, API
// tokens, chat on the owned flows, the comments and triage decisions the user wrote, organisation memberships,
// sessions and the personal Azure registry. ErrNotFound if the user does not exist. It runs on the caller's
// row-level-security scope.
func (b *PostgresStorageBackend) ExportUserData(ctx context.Context, userID string) (*interfaces.UserDataExport, error) {
	out := &interfaces.UserDataExport{}
	x := &exportRun{userID: userID}
	for _, s := range exportSections() {
		if err := s.fill(ctx, b, x, out); err != nil {
			return nil, fmt.Errorf("export: %s: %w", s.key, err)
		}
	}
	return out, nil
}

// ExportUserDataTo streams the same JSON bundle as ExportUserData to w, but fetches flows one page at a time, so
// peak memory is bounded to a single page's content. Callers MUST treat w as tainted if a non-nil error is returned
// (partial JSON may have been written) — the HTTP handler streams to a temp file and only forwards it on success,
// preserving the "complete or fail" guarantee a data-subject export requires.
func (b *PostgresStorageBackend) ExportUserDataTo(ctx context.Context, userID string, w io.Writer) error {
	bw := bufio.NewWriter(w)
	x := &exportRun{userID: userID}
	for i, s := range exportSections() {
		prefix := `,"`
		if i == 0 {
			prefix = `{"`
		}
		if _, err := bw.WriteString(prefix + s.key + `":`); err != nil {
			return err
		}
		if err := s.write(ctx, b, x, bw); err != nil {
			return fmt.Errorf("export: %s: %w", s.key, err)
		}
	}
	if _, err := bw.WriteString("}"); err != nil {
		return err
	}
	return bw.Flush()
}

func init() {
	registerLifecycle(lifecycleDomain{
		name: "bundle",
		export: []exportSection{
			section("exportedAt", func(context.Context, *PostgresStorageBackend, *exportRun) (time.Time, error) {
				return time.Now().UTC(), nil
			}, func(out *interfaces.UserDataExport, v time.Time) { out.ExportedAt = v }),
		},
	})
}
