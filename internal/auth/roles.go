package auth

// Role represents a user's role within the system or an organisation.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
	RoleGuest  Role = "guest"
)

// Permission is a fine-grained action on a resource, expressed as "resource:action".
type Permission string

const (
	// Flow permissions
	PermFlowRead   Permission = "flow:read"
	PermFlowWrite  Permission = "flow:write"
	PermFlowDelete Permission = "flow:delete"
	PermFlowShare  Permission = "flow:share"

	// Organisation permissions
	PermOrgRead   Permission = "org:read"
	PermOrgWrite  Permission = "org:write"
	PermOrgDelete Permission = "org:delete"
	PermOrgInvite Permission = "org:invite"

	// User / settings permissions
	PermSettingsRead  Permission = "settings:read"
	PermSettingsWrite Permission = "settings:write"
)

// rolePermissions maps each role to the set of permissions it grants.
var rolePermissions = map[Role][]Permission{
	RoleAdmin: {
		PermFlowRead, PermFlowWrite, PermFlowDelete, PermFlowShare,
		PermOrgRead, PermOrgWrite, PermOrgDelete, PermOrgInvite,
		PermSettingsRead, PermSettingsWrite,
	},
	RoleMember: {
		PermFlowRead, PermFlowWrite, PermFlowShare,
		PermOrgRead,
		PermSettingsRead, PermSettingsWrite,
	},
	RoleViewer: {
		PermFlowRead,
		PermOrgRead,
		PermSettingsRead,
	},
	RoleGuest: {
		PermFlowRead,
	},
}

// Permissions returns all permissions granted to the role.
func (r Role) Permissions() []Permission {
	return rolePermissions[r]
}

// Has reports whether the role grants the given permission.
func (r Role) Has(p Permission) bool {
	for _, granted := range rolePermissions[r] {
		if granted == p {
			return true
		}
	}
	return false
}

// IsValid reports whether r is a recognised role.
func (r Role) IsValid() bool {
	_, ok := rolePermissions[r]
	return ok
}
