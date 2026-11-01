package tool_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/tool"
)

func TestSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tenant := core.TenantID("00000000-0000-0000-0000-0000000000aa")
	for _, err := range []error{
		os.MkdirAll(filepath.Join(dir, string(tenant)), 0o700),
		os.WriteFile(filepath.Join(dir, "crm-token"), []byte("shared\n"), 0o600),
		os.WriteFile(filepath.Join(dir, string(tenant), "crm-token"), []byte("tenant's own\n"), 0o600),
		os.WriteFile(filepath.Join(filepath.Dir(dir), "outside"), []byte("not a secret"), 0o600),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	d := tool.DirSecrets{Dir: dir}
	for _, tenant := range []core.TenantID{"..", "../" + core.TenantID(filepath.Base(dir))} {
		if v, err := d.Secret(ctx, tenant, "outside"); err == nil {
			t.Fatalf("tenant %q read %q outside the directory", tenant, v)
		}
	}
	if v, err := d.Secret(ctx, tenant, "crm-token"); err != nil || v != "tenant's own" {
		t.Fatalf("tenant secret %q %v", v, err)
	}
	if v, err := d.Secret(ctx, core.DefaultTenant, "crm-token"); err != nil || v != "shared" {
		t.Fatalf("shared secret %q %v", v, err)
	}
	for _, name := range []string{"missing", "../crm-token", "a/b", ""} {
		if _, err := d.Secret(ctx, tenant, name); !errors.Is(err, tool.ErrNoSecret) {
			t.Errorf("%q: %v", name, err)
		}
	}
	t.Setenv("TEST_SECRET_CRM_TOKEN", "from env")
	e := tool.EnvSecrets{Prefix: "TEST_SECRET_"}
	if v, err := e.Secret(ctx, tenant, "crm-token"); err != nil || v != "from env" {
		t.Fatalf("env secret %q %v", v, err)
	}
	if _, err := e.Secret(ctx, tenant, "nope"); !errors.Is(err, tool.ErrNoSecret) {
		t.Fatalf("missing env secret: %v", err)
	}
}
