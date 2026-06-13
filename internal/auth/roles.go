package auth

type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
	RoleGuest  Role = "guest"
)

func (r Role) IsValid() bool {
	switch r {
	case RoleAdmin, RoleMember, RoleViewer, RoleGuest:
		return true
	}
	return false
}
