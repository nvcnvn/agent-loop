package policy

import (
	"fmt"
	"slices"

	"github.com/nvcnvn/agent-loop/core"
)

// Role is what a principal may do across its tenant.
type Role string

const (
	// RoleAdmin may do everything and read hidden reasoning.
	RoleAdmin Role = "admin"
	// RoleOperator may act on every session of the tenant and read internal events.
	RoleOperator Role = "operator"
	// RoleMember may create sessions and act in the sessions it participates in.
	RoleMember Role = "member"
	// RoleViewer may read the sessions it participates in.
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	return slices.Contains([]Role{RoleAdmin, RoleOperator, RoleMember, RoleViewer}, r)
}

// Operates reports whether r acts on every session of its tenant and sees the deployment's setup.
func (r Role) Operates() bool { return r == RoleAdmin || r == RoleOperator }

// CreatesSessions reports whether r may create sessions.
func (r Role) CreatesSessions() bool { return r.Valid() && r != RoleViewer }

// SessionRole is what a participant may do in one session.
type SessionRole string

const (
	// SessionOwner may also add and remove participants.
	SessionOwner SessionRole = "owner"
	// SessionMember may post messages, start and steer runs, and resolve approvals.
	SessionMember SessionRole = "member"
	// SessionViewer may only read.
	SessionViewer SessionRole = "viewer"
)

// Valid reports whether r is a known session role.
func (r SessionRole) Valid() bool {
	return r == SessionOwner || r == SessionMember || r == SessionViewer
}

// SessionAccess is what a principal may do in a session.
type SessionAccess struct {
	Read   bool // the session, its messages, runs, events and artifacts
	Write  bool // post messages, start, cancel and resume runs, resolve approvals
	Manage bool // add and remove participants
}

// Access combines a tenant role with a session role, empty for a non-participant.
func Access(role Role, participant SessionRole) SessionAccess {
	switch {
	case role.Operates():
		return SessionAccess{Read: true, Write: true, Manage: true}
	case participant == "":
		return SessionAccess{}
	case role == RoleViewer:
		return SessionAccess{Read: true}
	case role == RoleMember:
		return SessionAccess{Read: true, Write: participant != SessionViewer, Manage: participant == SessionOwner}
	}
	return SessionAccess{}
}

// Visibility maps each role to the event visibilities it may read.
type Visibility map[Role][]core.Visibility

// DefaultVisibility lets members and viewers read public and participant events, operators also internal and audit, admins all.
func DefaultVisibility() Visibility {
	shared := []core.Visibility{core.VisibilityPublic, core.VisibilityParticipant}
	return Visibility{
		RoleViewer:   shared,
		RoleMember:   shared,
		RoleOperator: append(slices.Clone(shared), core.VisibilityInternal, core.VisibilityAudit),
		RoleAdmin:    append(slices.Clone(shared), core.VisibilityInternal, core.VisibilityAudit, core.VisibilityHiddenReasoning),
	}
}

// Validate checks every role and visibility is a known one.
func (v Visibility) Validate() error {
	for role, list := range v {
		if !role.Valid() {
			return fmt.Errorf("visibility: unknown role %q", role)
		}
		for _, vis := range list {
			switch vis {
			case core.VisibilityPublic, core.VisibilityParticipant, core.VisibilityInternal, core.VisibilityHiddenReasoning, core.VisibilityAudit:
			default:
				return fmt.Errorf("visibility: unknown visibility %q for role %s", vis, role)
			}
		}
	}
	return nil
}

// Allows reports whether role may read events of visibility vis.
func (v Visibility) Allows(role Role, vis core.Visibility) bool {
	return slices.Contains(v[role], vis)
}

// HiddenPayload reports whether role may read events' hidden payloads, which only roles reading hidden_reasoning do.
func (v Visibility) HiddenPayload(role Role) bool {
	return v.Allows(role, core.VisibilityHiddenReasoning)
}
