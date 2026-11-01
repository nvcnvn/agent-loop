package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Participants checks session membership: the creator owns a session, roles
// change in place, and sessions and approvals can be listed by participant.
func Participants(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	owner, member := core.PrincipalID(core.NewID()), core.PrincipalID(core.NewID())
	other := core.TenantID(core.NewID())
	shared, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: owner, Project: "apollo"})
	if err != nil {
		t.Fatal(err)
	}
	if sess, err := s.GetSession(ctx, core.DefaultTenant, shared); err != nil || sess.Project != "apollo" {
		t.Fatalf("session=%+v err=%v", sess, err)
	}
	private, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.GetParticipant(ctx, core.DefaultTenant, shared, owner); err != nil || p.Role != "owner" {
		t.Fatalf("creator: %+v %v", p, err)
	}
	if err := s.PutParticipant(ctx, store.Participant{TenantID: core.DefaultTenant, SessionID: shared, PrincipalID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	joined, err := s.GetParticipant(ctx, core.DefaultTenant, shared, member)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutParticipant(ctx, store.Participant{TenantID: core.DefaultTenant, SessionID: shared, PrincipalID: member, Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.GetParticipant(ctx, core.DefaultTenant, shared, member); err != nil || p.Role != "viewer" || !p.JoinedAt.Equal(joined.JoinedAt) {
		t.Fatalf("after role change: %+v (joined %v) %v", p, joined.JoinedAt, err)
	}
	if list, err := s.ListParticipants(ctx, core.DefaultTenant, shared); err != nil || len(list) != 2 || list[0].PrincipalID != owner {
		t.Fatalf("participants: %+v %v", list, err)
	}
	if err := s.PutParticipant(ctx, store.Participant{TenantID: core.DefaultTenant, SessionID: shared, PrincipalID: member, Role: "boss"}); err == nil {
		t.Fatal("accepted an unknown session role")
	}
	if err := s.PutParticipant(ctx, store.Participant{TenantID: other, SessionID: shared, PrincipalID: member, Role: "member"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other tenant's put: %v", err)
	}
	if _, err := s.GetParticipant(ctx, other, shared, owner); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other tenant's get: %v", err)
	}
	if _, err := s.ListParticipants(ctx, other, shared); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other tenant's list: %v", err)
	}

	sessionIDs := func(p core.PrincipalID) []core.SessionID {
		t.Helper()
		list, err := s.ListSessions(ctx, core.DefaultTenant, p)
		if err != nil {
			t.Fatal(err)
		}
		var ids []core.SessionID
		for _, sess := range list {
			ids = append(ids, sess.ID)
		}
		return ids
	}
	if ids := sessionIDs(member); !slices.Equal(ids, []core.SessionID{shared}) {
		t.Fatalf("member's sessions: %v", ids)
	}
	if list, _ := s.ListSessions(ctx, core.DefaultTenant, member); len(list) != 1 || list[0].Project != "apollo" {
		t.Fatalf("listed sessions lose their project: %+v", list)
	}
	if ids := sessionIDs(""); !slices.Contains(ids, shared) || !slices.Contains(ids, private) {
		t.Fatalf("all sessions: %v", ids)
	}

	sharedRun, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: shared})
	if err != nil {
		t.Fatal(err)
	}
	privateRun, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: private})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: sharedRun, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: privateRun, Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{Participant: member})
	if err != nil || len(list) != 1 || list[0].ID != visible {
		t.Fatalf("member's approvals: %+v %v", list, err)
	}

	if err := s.RemoveParticipant(ctx, core.DefaultTenant, shared, member); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveParticipant(ctx, core.DefaultTenant, shared, member); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second removal: %v", err)
	}
	if ids := sessionIDs(member); len(ids) != 0 {
		t.Fatalf("removed member's sessions: %v", ids)
	}
}
