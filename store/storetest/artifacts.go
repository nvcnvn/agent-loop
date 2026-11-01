package storetest

import (
	"context"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Artifacts checks an ArtifactStore keeps what produced an artifact and lists
// a run's artifacts in order, for its tenant only. The IDs must be UUIDs of a
// session and run the driver accepts.
func Artifacts(t *testing.T, s store.ArtifactStore, session core.SessionID, run core.RunID) {
	t.Helper()
	ctx := context.Background()
	want := store.Artifact{
		TenantID: core.DefaultTenant, SessionID: session, RunID: run, TaskID: core.TaskID(core.NewID()), StepID: core.StepID(core.NewID()),
		Producer: "artifact.save", Kind: "report", Name: "report.md", MimeType: "text/markdown", Content: []byte("# done"),
		Metadata: map[string]any{"lines": 1.0},
	}
	first, err := s.PutArtifact(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutArtifact(ctx, store.Artifact{TenantID: core.DefaultTenant, SessionID: session, RunID: run, Kind: "file", Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetArtifact(ctx, core.DefaultTenant, first)
	if err != nil || got.StepID != want.StepID || got.TaskID != want.TaskID || got.Producer != want.Producer || got.Kind != want.Kind ||
		got.Name != want.Name || got.MimeType != want.MimeType || string(got.Content) != "# done" || got.Size != 6 || got.Metadata["lines"] != 1.0 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if _, err := s.GetArtifact(ctx, core.TenantID(core.NewID()), first); err == nil {
		t.Fatal("another tenant read the artifact")
	}
	list, err := s.ListRunArtifacts(ctx, core.DefaultTenant, run)
	if err != nil || len(list) != 2 || list[0].ID != first || list[1].ID != second || list[1].MimeType != "application/octet-stream" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}
