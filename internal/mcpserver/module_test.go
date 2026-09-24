package mcpserver

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

func TestProvisions_moduleService(t *testing.T) {
	db := svc("go", "db", &alphasfile.ProvisionStep{Name: "seed", Cmd: "seed.sh"})
	db.Module = "payments"

	got := Provisions([]*alphasfile.Service{db})
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	p := got[0]
	if p.ID != "module.payments.service.go.db.runtime.provision.seed" {
		t.Errorf("ID = %q", p.ID)
	}
	if p.Module != "payments" || p.Service != "payments/db" {
		t.Errorf("Module=%q Service=%q", p.Module, p.Service)
	}
	if got, want := p.ToolName(), "provision__go_payments_db__seed"; got != want {
		t.Errorf("ToolName = %q, want %q", got, want)
	}
}

func TestProvisionID_defaultModuleUnchanged(t *testing.T) {
	if got := ProvisionID("go", "db", "seed"); got != "service.go.db.runtime.provision.seed" {
		t.Errorf("got %q", got)
	}
}
