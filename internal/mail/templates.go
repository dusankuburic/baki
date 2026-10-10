package mail

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	"slices"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"
	"unicode/utf8"
)

// Locale selects the language of a rendered email. Only English exists: every locale renders the "en" bodies until
// a second language ships, and Message.Locale records what was used.
type Locale string

// DefaultLocale is what an empty or unknown Locale renders.
const DefaultLocale Locale = "en"

func (l Locale) resolve() Locale {
	if l == DefaultLocale {
		return l
	}
	return DefaultLocale // "" and every language without bodies yet
}

// Template declares one email the product sends. Subject and Text are text/template sources, HTML an html/template
// source (contextual escaping: user text in an element, a link in an href). Each executes against
// {.Data D, .App Env, .Locale}. Sensitive marks a body that carries a credential (a one-time token link): such mail
// keeps a reserved share of the mail runner, so a burst of notifications cannot crowd out a password reset.
type Template[D any] struct {
	ID        string
	Subject   string
	Text      string
	HTML      string
	Sensitive bool
}

// Registered is a parsed template in the registry; Register returns it.
type Registered[D any] struct {
	id        string
	sensitive bool
	subject   *texttemplate.Template
	text      *texttemplate.Template
	html      *htmltemplate.Template
}

// Env is what a template sees besides its data: the deployment's public base URL (no trailing slash, "" when unset).
type Env struct {
	BaseURL string
}

// TokenLink is the frontend deep link carrying a one-time token, or the bare token without a base URL so it stays
// recoverable (the log-only mailer in development).
func (e Env) TokenLink(param, token string) string {
	if e.BaseURL == "" {
		return token
	}
	return e.BaseURL + "/#" + param + "=" + token
}

// AppLink is the app's entry point, "" without a base URL (a relative link in an email resolves against the mail
// client, so templates omit it).
func (e Env) AppLink() string {
	if e.BaseURL == "" {
		return ""
	}
	return e.BaseURL + "/"
}

// view is the dot of every template execution.
type view[D any] struct {
	Data   D
	App    Env
	Locale Locale
}

// Message is a rendered email.
type Message struct {
	Template  string
	Locale    Locale
	Sensitive bool
	Subject   string
	Text      string
	HTML      string
}

// Mail is a registered template bound to its data, ready for Service.Enqueue. Locale "" is DefaultLocale.
type Mail struct {
	Locale Locale
	t      entry
	data   any
}

// entry is the type-erased view of a Registered template.
type entry interface {
	ID() string
	renderAny(data any, env Env, loc Locale) (Message, error)
}

var registry = struct {
	sync.Mutex
	byID map[string]entry
}{byID: map[string]entry{}}

var funcs = map[string]any{
	"upper":   strings.ToUpper,
	"preview": previewOf,
	"rfc3339": func(t time.Time) string { return t.Format(time.RFC3339) },
}

// Register parses t, checks it executes against a zero D, and adds it to the registry. It panics on a bad template
// or a duplicate ID: templates are package-level declarations, so the process fails at init, not at the first send.
func Register[D any](t Template[D]) *Registered[D] {
	if t.ID == "" || t.Subject == "" || t.Text == "" {
		panic(fmt.Sprintf("mail: template %q needs an ID, a subject and a text body", t.ID))
	}
	r := &Registered[D]{
		id:        t.ID,
		sensitive: t.Sensitive,
		subject:   texttemplate.Must(texttemplate.New(t.ID + ".subject").Funcs(funcs).Parse(t.Subject)),
		text:      texttemplate.Must(texttemplate.New(t.ID + ".txt").Funcs(funcs).Parse(t.Text)),
	}
	if t.HTML != "" {
		r.html = htmltemplate.Must(htmltemplate.New(t.ID + ".html").Funcs(funcs).Parse(t.HTML))
	}
	var zero D
	if _, err := r.Render(zero, Env{}, DefaultLocale); err != nil {
		panic(fmt.Sprintf("mail: template %q: %v", t.ID, err))
	}
	registry.Lock()
	defer registry.Unlock()
	if _, dup := registry.byID[t.ID]; dup {
		panic(fmt.Sprintf("mail: template %q registered twice", t.ID))
	}
	registry.byID[t.ID] = r
	return r
}

// IDs lists the registered template IDs, sorted.
func IDs() []string {
	registry.Lock()
	defer registry.Unlock()
	ids := make([]string, 0, len(registry.byID))
	for id := range registry.byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func lookup(id string) (entry, bool) {
	registry.Lock()
	defer registry.Unlock()
	e, ok := registry.byID[id]
	return e, ok
}

// ID is the template's registry key (stable: goldens and logs use it).
func (r *Registered[D]) ID() string { return r.id }

// With binds data to the template for Service.Enqueue.
func (r *Registered[D]) With(data D) Mail { return Mail{t: r, data: data} }

// Render executes the template. The subject is folded to one line and capped at maxSubjectRunes.
func (r *Registered[D]) Render(data D, env Env, loc Locale) (Message, error) {
	loc = loc.resolve()
	v := view[D]{Data: data, App: env, Locale: loc}
	msg := Message{Template: r.id, Locale: loc, Sensitive: r.sensitive}
	var b bytes.Buffer
	if err := r.subject.Execute(&b, v); err != nil {
		return Message{}, fmt.Errorf("mail: render %s subject: %w", r.id, err)
	}
	msg.Subject = oneLine(b.String())
	b.Reset()
	if err := r.text.Execute(&b, v); err != nil {
		return Message{}, fmt.Errorf("mail: render %s text: %w", r.id, err)
	}
	msg.Text = b.String()
	if r.html != nil {
		b.Reset()
		if err := r.html.Execute(&b, v); err != nil {
			return Message{}, fmt.Errorf("mail: render %s html: %w", r.id, err)
		}
		msg.HTML = b.String()
	}
	return msg, nil
}

func (r *Registered[D]) renderAny(data any, env Env, loc Locale) (Message, error) {
	d, ok := data.(D)
	if !ok {
		return Message{}, fmt.Errorf("mail: template %s takes %T, got %T", r.id, *new(D), data)
	}
	return r.Render(d, env, loc)
}

func (m Mail) render(env Env) (Message, error) {
	if m.t == nil {
		return Message{}, fmt.Errorf("mail: no template")
	}
	return m.t.renderAny(m.data, env, m.Locale)
}

// maxSubjectRunes caps a subject (they carry flow names): the header stays far below the 998-byte line limit even
// fully Q-encoded once folded.
const maxSubjectRunes = 200

// oneLine turns a rendered subject into a single header-safe line: every run of white space (CR and LF included)
// becomes one space, and an over-long subject is cut on a rune boundary.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxSubjectRunes {
		return s
	}
	return string([]rune(s)[:maxSubjectRunes-1]) + "…"
}
