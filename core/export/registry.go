package export

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"sync/atomic"

	"pad-core/models"
)

// Input is what a format renders. A nil Report renders as an empty report;
// Doc may be nil (no source lines, file names or member files).
type Input struct {
	Report *models.AnalysisReport
	Doc    *models.FlowDocument
}

// Format is one registered export format. A new format is one file whose
// init() calls Register.
type Format struct {
	// ID is the wire id: the CLI -format value, the API "format" field and the
	// audit Meta["format"] of stored rows. Lower-case; never renamed.
	ID string
	// Label is the human name for help text and menus.
	Label string
	// MIME is the full Content-Type value.
	MIME string
	// Ext is the file extension, with the dot.
	Ext string
	// Binary formats are not text: base64 in JSON envelopes, never written to
	// a terminal.
	Binary bool
	// Order is the format's position in List and IDs (unique, positive).
	Order int
	// Render writes the complete document to w. The built-in formats render
	// into memory and write once, so an error leaves w untouched.
	Render func(w io.Writer, in Input, opts Options) error
}

// Bytes renders the document into memory through f.Render, so a Render
// replaced on a copy of a Format is the one that runs.
func (f Format) Bytes(in Input, opts Options) ([]byte, error) {
	var sink byteSink
	if err := f.Render(&sink, in, opts); err != nil {
		return nil, err
	}
	return sink.b, nil
}

// byteSink is the writer behind Bytes. A built-in Render that finds it empty
// hands over its rendered slice instead of copying it.
type byteSink struct{ b []byte }

func (s *byteSink) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *byteSink) WriteString(p string) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

// ErrUnknownFormat is returned (wrapped) by Bytes for an unregistered ID.
var ErrUnknownFormat = errors.New("unknown export format")

// Register adds f to the format registry. Call it from an init() only: it
// panics on an invalid or duplicate ID, Ext or Order, a missing field, or a
// call after the registry was first read.
func Register(f Format) { formats.register(f) }

// Lookup returns the format registered under id (case-sensitive).
func Lookup(id string) (Format, bool) {
	formats.sealed.Store(true)
	f, ok := formats.byID[id]
	return f, ok
}

// List returns every registered format by Order. The slice is a copy.
func List() []Format {
	formats.sealed.Store(true)
	return append([]Format(nil), formats.list...)
}

// IDs returns the registered format IDs by Order.
func IDs() []string {
	formats.sealed.Store(true)
	ids := make([]string, len(formats.list))
	for i, f := range formats.list {
		ids[i] = f.ID
	}
	return ids
}

// Bytes renders in as the format id.
func Bytes(id string, in Input, opts Options) ([]byte, error) {
	f, ok := Lookup(id)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownFormat, id)
	}
	return f.Bytes(in, opts)
}

// registerBuiltin registers a format rendered in memory by render: Render
// writes render's bytes once, or hands them to an empty Bytes sink.
func registerBuiltin(f Format, render func(in Input, opts Options) ([]byte, error)) {
	f.Render = func(w io.Writer, in Input, opts Options) error {
		b, err := render(in, opts)
		if err != nil {
			return err
		}
		if s, ok := w.(*byteSink); ok && s.b == nil {
			s.b = b
			return nil
		}
		_, err = w.Write(b)
		return err
	}
	Register(f)
}

type registry struct {
	byID map[string]Format
	list []Format // by Order
	// sealed is set by the first read: registration is init-only, which is
	// what makes the lock-free reads safe.
	sealed atomic.Bool
}

var formats = &registry{byID: map[string]Format{}}

var (
	formatIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	formatExtPattern = regexp.MustCompile(`^\.[a-z0-9][a-z0-9.+-]*$`)
)

func (r *registry) register(f Format) {
	fail := func(msg string) { panic(fmt.Sprintf("export.Register(%q): %s", f.ID, msg)) }
	switch {
	case r.sealed.Load():
		fail("the registry was already read; register from init()")
	case !formatIDPattern.MatchString(f.ID):
		fail("ID must be lower-case letters, digits and dashes")
	case f.ID == "text":
		fail(`"text" is the CLI's console output, not an export format`)
	case f.Label == "" || f.MIME == "":
		fail("Label and MIME are required")
	case !formatExtPattern.MatchString(f.Ext):
		fail("Ext must be a lower-case extension with the dot")
	case f.Order <= 0:
		fail("Order must be positive")
	case f.Render == nil:
		fail("Render is required")
	}
	if _, dup := r.byID[f.ID]; dup {
		fail("duplicate ID")
	}
	for _, o := range r.list {
		switch {
		case o.Ext == f.Ext:
			fail(fmt.Sprintf("Ext %s is taken by %q", f.Ext, o.ID))
		case o.Order == f.Order:
			fail(fmt.Sprintf("Order %d is taken by %q", f.Order, o.ID))
		}
	}
	r.byID[f.ID] = f
	r.list = append(r.list, f)
	sort.Slice(r.list, func(i, j int) bool { return r.list[i].Order < r.list[j].Order })
}
