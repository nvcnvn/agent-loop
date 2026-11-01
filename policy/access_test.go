package policy_test

import (
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/policy"
)

func TestAccess(t *testing.T) {
	for _, tc := range []struct {
		role        policy.Role
		participant policy.SessionRole
		want        policy.SessionAccess
	}{
		{policy.RoleAdmin, "", policy.SessionAccess{Read: true, Write: true, Manage: true}},
		{policy.RoleOperator, "", policy.SessionAccess{Read: true, Write: true, Manage: true}},
		{policy.RoleMember, "", policy.SessionAccess{}},
		{policy.RoleMember, policy.SessionViewer, policy.SessionAccess{Read: true}},
		{policy.RoleMember, policy.SessionMember, policy.SessionAccess{Read: true, Write: true}},
		{policy.RoleMember, policy.SessionOwner, policy.SessionAccess{Read: true, Write: true, Manage: true}},
		{policy.RoleViewer, policy.SessionOwner, policy.SessionAccess{Read: true}},
		{policy.RoleViewer, "", policy.SessionAccess{}},
		{"", policy.SessionOwner, policy.SessionAccess{}},
	} {
		if got := policy.Access(tc.role, tc.participant); got != tc.want {
			t.Errorf("Access(%q, %q) = %+v, want %+v", tc.role, tc.participant, got, tc.want)
		}
	}
}

func TestVisibility(t *testing.T) {
	v := policy.DefaultVisibility()
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if v.Allows(policy.RoleMember, core.VisibilityInternal) || !v.Allows(policy.RoleOperator, core.VisibilityAudit) || v.Allows(policy.RoleOperator, core.VisibilityHiddenReasoning) || !v.Allows(policy.RoleAdmin, core.VisibilityHiddenReasoning) {
		t.Fatalf("default visibility: %v", v)
	}
	for _, bad := range []policy.Visibility{{"root": nil}, {policy.RoleMember: {"secret"}}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
