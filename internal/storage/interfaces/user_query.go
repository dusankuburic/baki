package interfaces

import (
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
)

// UserRoleNone is the role filter (and stats bucket) for users whose stored
// role is not one of the four valid roles — the admin list shows them as
// unassigned so an operator can give them a role.
const UserRoleNone = "none"

// UserQuery filters the admin user list. Q is a case-insensitive substring of
// the email or display name, a prefix of either when it has no run of
// MinSearchTermRunes word characters (ShortSearchTerm: no trigram index
// serves such a substring) or Prefix is set; Role is "" (every user), a valid
// auth.Role, or UserRoleNone; Limit/Offset page the ordered result.
type UserQuery struct {
	Q      string
	Role   string
	Limit  int
	Offset int
	// After is a keyset cursor (the CreatedAt and ID of the last user of the page before): the page holds the users
	// after it in the list's order (created_at desc, id asc), and Offset is ignored. The total still counts every match.
	After *PageCursor
	// Prefix matches Q as a prefix even when ShortSearchTerm calls it long: the Postgres store sets it for a term
	// its database draws no trigram from.
	Prefix bool
}

// Matches reports whether u satisfies the query's Q and Role filters.
func (q UserQuery) Matches(u *User) bool {
	if needle := strings.ToLower(strings.TrimSpace(q.Q)); needle != "" {
		match := strings.Contains
		if q.Prefix || ShortSearchTerm(needle) {
			match = strings.HasPrefix
		}
		if !match(strings.ToLower(u.Email), needle) && !match(strings.ToLower(u.DisplayName), needle) {
			return false
		}
	}
	switch q.Role {
	case "":
		return true
	case UserRoleNone:
		return !u.Role.IsValid()
	default:
		return string(u.Role) == q.Role
	}
}

// Apply runs the query over an in-memory user set: keeps what Matches accepts,
// orders newest first (created_at desc, id asc — the Postgres order), returns
// the Offset/Limit page (or the Limit after the After cursor) plus the match
// count. Non-positive Limit takes everything after Offset; the page is never nil.
func (q UserQuery) Apply(users []*User) ([]*User, int) {
	matched := make([]*User, 0, len(users))
	for _, u := range users {
		if q.Matches(u) {
			matched = append(matched, u)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID < matched[j].ID
	})
	total := len(matched)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if a := q.After; a != nil {
		// The first user after the cursor: older, or as old with a greater id.
		offset = sort.Search(total, func(i int) bool {
			u := matched[i]
			return u.CreatedAt.Before(a.At) || (u.CreatedAt.Equal(a.At) && u.ID > a.ID)
		})
	}
	if offset >= total {
		return []*User{}, total
	}
	page := matched[offset:]
	if q.Limit > 0 && q.Limit < len(page) {
		page = page[:q.Limit]
	}
	return page, total
}

// UserStats counts every user by role and state for the admin overview.
// ByRole carries the four valid roles plus UserRoleNone.
type UserStats struct {
	Total      int
	ByRole     map[string]int
	Unverified int
	Locked     int
}

// UserStatsOf computes UserStats over an in-memory user set; a user is locked
// while LockedUntil lies after now.
func UserStatsOf(users []*User, now time.Time) *UserStats {
	s := &UserStats{ByRole: map[string]int{
		string(auth.RoleAdmin):  0,
		string(auth.RoleMember): 0,
		string(auth.RoleViewer): 0,
		string(auth.RoleGuest):  0,
		UserRoleNone:            0,
	}}
	for _, u := range users {
		s.Total++
		if u.Role.IsValid() {
			s.ByRole[string(u.Role)]++
		} else {
			s.ByRole[UserRoleNone]++
		}
		if !u.EmailVerified {
			s.Unverified++
		}
		if u.LockedUntil != nil && u.LockedUntil.After(now) {
			s.Locked++
		}
	}
	return s
}
