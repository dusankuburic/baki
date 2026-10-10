package cache

// PartKind is what a key part stands for. A spec declares the kinds its keys carry (Keys), and Key accepts exactly
// those.
type PartKind uint8

const (
	KindTenant      PartKind = iota + 1 // whose data the value derives from, or who may see it
	KindRuleProfile                     // digest of the analysis configuration the value was computed under
	KindContent                         // digest of the input content (and its parse identity)
	KindResource                        // a stored resource id (flow id); access is re-checked on every read
	KindVersion                         // code or format version of the computation, or an edit generation
	KindParam                           // any other input (provider, model, query, flags)
	maxKind         = KindParam
)

var kindNames = [...]string{
	KindTenant:      "tenant",
	KindRuleProfile: "rule-profile",
	KindContent:     "content",
	KindResource:    "resource",
	KindVersion:     "version",
	KindParam:       "param",
}

// kindCodes are the one-byte kind tags of the key encoding.
var kindCodes = [...]byte{
	KindTenant:      't',
	KindRuleProfile: 'p',
	KindContent:     'c',
	KindResource:    'r',
	KindVersion:     'v',
	KindParam:       'a',
}

func (k PartKind) valid() bool { return k >= KindTenant && k <= maxKind }

func (k PartKind) String() string {
	if !k.valid() {
		return "invalid"
	}
	return kindNames[k]
}

func kindOfCode(b byte) PartKind {
	for k := KindTenant; k <= maxKind; k++ {
		if kindCodes[k] == b {
			return k
		}
	}
	return 0
}

// Part is one component of a key. The zero Part is invalid: constructors return it for empty input, and Key
// refuses it, so an empty tenant or digest never silently widens a key.
type Part struct {
	kind PartKind
	name string
	val  string
}

// Tenant part names: which kind of tenant a KindTenant part identifies.
const (
	tenantOrg     = "org"
	tenantUser    = "user"
	tenantOrgUser = "org-user"
	tenantLocal   = "local"
)

// Org is the tenant part of a value derived from (or visible to) one organisation.
func Org(orgID string) Part { return part(KindTenant, tenantOrg, orgID) }

// User is the tenant part of a value derived from (or visible to) one user.
func User(userID string) Part { return part(KindTenant, tenantUser, userID) }

// OrgUser is the tenant part of a view of one user inside one organisation (a value that depends on the caller
// as well as on the organisation's data). Zero unless both are set.
func OrgUser(orgID, userID string) Part {
	if orgID == "" || userID == "" {
		return Part{}
	}
	return Part{kind: KindTenant, name: tenantOrgUser, val: lenPrefixed(orgID) + userID}
}

// Tenant is Org(orgID) when it is set, else User(userID): the owner of a stored resource. Zero when both are empty.
func Tenant(orgID, userID string) Part {
	if orgID != "" {
		return Org(orgID)
	}
	return User(userID)
}

// Local is the tenant part of the single user of the desktop app and of local mode without accounts.
func Local() Part { return Part{kind: KindTenant, name: tenantLocal} }

// RuleProfile is the digest of the analysis configuration (rule set and settings) a value was computed under.
func RuleProfile(digest string) Part { return part(KindRuleProfile, "", digest) }

// Content is the digest of the input a value was computed from.
func Content(digest string) Part { return part(KindContent, "", digest) }

// Resource is the id of a stored resource; whoever reads the cached value has passed that resource's access check.
func Resource(id string) Part { return part(KindResource, "", id) }

// Version is the version of the computation or of its input (a code version, an edit generation).
func Version(v string) Part { return part(KindVersion, "", v) }

// Param is any other input of the value, named. Unlike the other parts its value may be empty (an unset flag or
// option is an input too); the name may not.
func Param(name, value string) Part {
	if name == "" {
		return Part{}
	}
	return Part{kind: KindParam, name: name, val: value}
}

func part(kind PartKind, name, val string) Part {
	if val == "" {
		return Part{}
	}
	return Part{kind: kind, name: name, val: val}
}

// IsZero reports whether p is the invalid zero Part (built from empty input).
func (p Part) IsZero() bool { return p.kind == 0 }

// Kind is what the part stands for.
func (p Part) Kind() PartKind { return p.kind }

// Name is the tenant kind ("org", "user", "org-user", "local") of a tenant part, the name of a Param, and empty
// for the other kinds.
func (p Part) Name() string { return p.name }

// Value is the part's value as it is encoded into the key.
func (p Part) Value() string { return p.val }

func (p Part) String() string {
	if p.IsZero() {
		return "<zero>"
	}
	if p.name == "" {
		return p.kind.String() + "=" + p.val
	}
	return p.kind.String() + ":" + p.name + "=" + p.val
}
