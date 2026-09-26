package interfaces

import (
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
)

// UserRoleNone is the role filter (and stats bucket) for users whose stored
// role is not one of the four valid roles: accounts written before the role
// check existed, or by the storage contract suite. The admin list shows them
// as unassigned so an operator can give them a role.
const UserRoleNone = "none"

// UserQuery filters the admin user list. Q is matched case-insensitively as a
// substring of the email or the display name; Role is "" (every user), a valid
// auth.Role, or UserRoleNone. Limit and Offset page the ordered result.
type UserQuery struct {
	Q      string
	Role   string
	Limit  int
	Offset int
}

// Matches reports whether u satisfies the query's Q and Role filters.
func (q UserQuery) Matches(u *User) bool {
	if needle := strings.ToLower(strings.TrimSpace(q.Q)); needle != "" {
		if !strings.Contains(strings.ToLower(u.Email), needle) &&
			!strings.Contains(strings.ToLower(u.DisplayName), needle) {
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

// Apply runs the query over an in-memory user set: it keeps the users Matches
// accepts, orders them newest first (created_at descending, id ascending as the
// tiebreaker — the order the Postgres backend uses), and returns the page
// Offset/Limit select together with the match count. A non-positive Limit
// takes everything after Offset. The page is never nil.
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
