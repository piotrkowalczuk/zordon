package alphasfile

import (
	"strings"
	"testing"
)

// A latent provider + a cross-service invoker resolve to the expected
// Latent / CmdRef wire shape, and a non-referencing provision keeps its
// inline cmd plus the implicit self.runtime@ready default.
func TestResolveLatentAndCmdRef(t *testing.T) {
	af := compile(t, `
service "go" "kafka" {
  src { path = "../.." }
  runtime {
    cmd = ["./kafka"]
    provision "create-topic" {
      after = never
      cmd   = "echo $TOPIC"
    }
  }
}

service "go" "app" {
  src { path = "../.." }
  runtime {
    cmd = ["./app"]
    provision "topic" {
      cmd = service.go.kafka.runtime.provision.create-topic
      env = { TOPIC = "app-events" }
    }
    provision "plain" {
      cmd = "true"
    }
  }
}
`, nil)

	tpl := provByName(svcByName(af, "kafka"), "create-topic")
	if !tpl.Latent || tpl.CmdRef != "" || len(tpl.After) != 0 {
		t.Fatalf("template = %+v; want Latent, no CmdRef, empty After", tpl)
	}
	inv := provByName(svcByName(af, "app"), "topic")
	if inv.CmdRef != "service.go.kafka.runtime.provision.create-topic" || inv.Cmd != "" {
		t.Fatalf("invoker = %+v; want CmdRef set, empty Cmd", inv)
	}
	if len(inv.After) != 0 {
		t.Errorf("invoker.After = %v; want empty (no implicit default — starts immediately)", inv.After)
	}
	plain := provByName(svcByName(af, "app"), "plain")
	if plain.Cmd != "true" || plain.CmdRef != "" {
		t.Errorf("plain = %+v; want inline cmd, no CmdRef", plain)
	}
	if len(plain.After) != 0 {
		t.Errorf("plain.After = %v; want empty (no implicit ready default)", plain.After)
	}
}

// `never` is not required to be a template: a CmdRef may target an
// ordinary (non-latent) provision. It compiles, the target keeps its
// inline cmd (and still auto-runs), and the invoker gets the CmdRef.
func TestResolveCmdRefToNonLatent(t *testing.T) {
	af := compile(t, `
service "go" "prov" {
  src { path = "../.." }
  runtime {
    cmd = ["./prov"]
    provision "seed" {
      cmd = "echo hi"
    }
  }
}

service "go" "cons" {
  src { path = "../.." }
  runtime {
    cmd = ["./cons"]
    provision "seed-mine" {
      cmd = service.go.prov.runtime.provision.seed
    }
  }
}
`, nil)

	seed := provByName(svcByName(af, "prov"), "seed")
	if seed.Latent || seed.Cmd != "echo hi" {
		t.Fatalf("seed = %+v; want non-latent with inline cmd", seed)
	}
	if len(seed.After) != 0 {
		t.Errorf("seed.After = %v; want empty (auto-runs immediately, no implicit default)", seed.After)
	}
	mine := provByName(svcByName(af, "cons"), "seed-mine")
	if mine.CmdRef != "service.go.prov.runtime.provision.seed" || mine.Cmd != "" {
		t.Errorf("seed-mine = %+v; want CmdRef to non-latent seed", mine)
	}
}

func TestResolveInvokeErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			name: "never_with_other_refs",
			src: `
service "go" "x" {
  src { path = "../.." }
  runtime {
    cmd = ["./x"]
    provision "a" { cmd = "true" }
    provision "b" {
      after = [never, self.runtime.provision.a.success]
      cmd   = "true"
    }
  }
}`,
			want: "cannot be combined",
		},
		{
			name: "check_with_cmd_ref",
			src: `
service "go" "k" {
  src { path = "../.." }
  runtime {
    cmd = ["./k"]
    provision "tpl" {
      after = never
      cmd   = "true"
    }
  }
}
service "go" "c" {
  src { path = "../.." }
  runtime {
    cmd = ["./c"]
    provision "use" {
      cmd   = service.go.k.runtime.provision.tpl
      check = "test -e /x"
    }
  }
}`,
			want: "check/verify are not allowed",
		},
		{
			name: "detached_latent",
			src: `
service "go" "x" {
  src { path = "../.." }
  runtime {
    cmd = ["./x"]
    provision "a" {
      after    = never
      detached = true
      cmd      = "true"
    }
  }
}`,
			want: "detached",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Compile("t", []byte(c.src), testInv(), nil, TestConfig{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
