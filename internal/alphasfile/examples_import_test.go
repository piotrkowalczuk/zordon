package alphasfile

import (
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// examples/import: the entrypoint imports app and billing from one
// fragment, which imports kafka from another. Resolved through the real
// files, no process spawned.
func TestExampleImportResolves(t *testing.T) {
	root, err := filepath.Abs("../../examples/import/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	exDir := filepath.Dir(root)
	repo := filepath.Dir(filepath.Dir(exDir))
	if got := len(tree.Imports()); got != 2 {
		t.Fatalf("imports = %+v", tree.Imports())
	}
	if u := tree.Unused(); len(u) != 1 || u[0].Module != "kafka-ui" {
		t.Errorf("unused = %+v", u)
	}

	iv := &invocation.InvocationState{
		FsHash: "abc0000011112222", TmpDir: "/tmp/zordon-abc0000011112222",
		Workspace: invocation.MainWorkspace, StateDir: filepath.Join(exDir, "workspaces/main"),
	}
	af, err := Resolve(tree, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(af); len(got) != 3 {
		t.Fatalf("services = %v", got)
	}
	kafka := svcByName(af, "kafka/kafka")
	if want := filepath.Join(repo, "examples/import/src/kafka"); kafka.Runtime.Dir != want {
		t.Errorf("kafka dir = %q, want %q", kafka.Runtime.Dir, want)
	}
	if !provByName(kafka, "create-topic").Latent {
		t.Error("create-topic must stay latent")
	}
	for _, name := range []string{"app/app", "billing/billing"} {
		topic := provByName(svcByName(af, name), "topic")
		if topic == nil || topic.CmdRef != "module.kafka.service.go.kafka.runtime.provision.create-topic" {
			t.Errorf("%s topic provision = %+v", name, topic)
		}
	}
}
