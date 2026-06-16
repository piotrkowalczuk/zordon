package alphasfile

import (
	"strings"
	"testing"
)

// TestProvisionClean_resolves pins that a provision's `clean` snippet is
// parsed and interpolated in the same scope as cmd/verify, and that omitting
// it leaves Clean empty (so `zordon clean` skips that provision).
func TestProvisionClean_resolves(t *testing.T) {
	src := `
service "go" "db" {
  git { url = "github.com/acme/db" }
  vars = { name = "appdb" }
  runtime {
    cmd = ["./db"]
    provision "create" {
      cmd   = "createdb ${self.vars.name}"
      clean = "dropdb ${self.vars.name}"
    }
    provision "noclean" {
      cmd = "echo hi"
    }
  }
}
`
	af := compile(t, src, nil)
	db := svcByName(af, "db")
	if db == nil {
		t.Fatal("service db not resolved")
	}
	create := provByName(db, "create")
	if create == nil {
		t.Fatal("provision create not resolved")
	}
	if got, want := create.Clean, "dropdb appdb"; got != want {
		t.Errorf("create.Clean = %q; want %q (interpolated against self.vars)", got, want)
	}
	if noclean := provByName(db, "noclean"); noclean == nil {
		t.Fatal("provision noclean not resolved")
	} else if noclean.Clean != "" {
		t.Errorf("noclean.Clean = %q; want empty (no clean declared)", noclean.Clean)
	}
}

// TestProvisionClean_allowedWithCmdRef pins that `clean` — unlike
// check/verify — is allowed on a provision whose cmd references another
// provision: clean is the invoker's own teardown, not the referenced
// template's, so it carries the invoker's env/scope.
func TestProvisionClean_allowedWithCmdRef(t *testing.T) {
	src := `
service "go" "kafka" {
  git { url = "github.com/acme/kafka" }
  vars = { topics = "/tmp/topics.txt" }
  runtime {
    cmd = ["./kafka"]
    provision "create-topic" {
      after = never
      cmd   = "echo $TOPIC >> ${self.vars.topics}"
    }
  }
}

service "go" "app" {
  git { url = "github.com/acme/app" }
  runtime {
    cmd = ["./app"]
    provision "topic" {
      after = [self.runtime.ready]
      cmd   = service.go.kafka.runtime.provision.create-topic
      clean = "echo del app-events"
      env   = { TOPIC = "app-events" }
    }
  }
}
`
	af := compile(t, src, nil)
	topic := provByName(svcByName(af, "app"), "topic")
	if topic == nil {
		t.Fatal("app.topic not resolved")
	}
	if topic.CmdRef == "" {
		t.Error("expected CmdRef to be set on a cmd-ref invoker")
	}
	if strings.TrimSpace(topic.Cmd) != "" {
		t.Errorf("cmd-ref invoker should have no inline cmd; got %q", topic.Cmd)
	}
	if topic.Clean != "echo del app-events" {
		t.Errorf("topic.Clean = %q; want the invoker's own teardown snippet", topic.Clean)
	}
}
