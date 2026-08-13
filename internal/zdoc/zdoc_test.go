package zdoc

import (
	"strings"
	"testing"
)

func TestFormatOf(t *testing.T) {
	cases := map[string]struct {
		path string
		want Format
		ok   bool
	}{
		"json":            {path: ".claude/settings.json", want: FormatJSON, ok: true},
		"yaml":            {path: "compose.yaml", want: FormatYAML, ok: true},
		"yml":             {path: "compose.yml", want: FormatYAML, ok: true},
		"toml":            {path: "config.toml", want: FormatTOML, ok: true},
		"uppercase":       {path: "Config.TOML", want: FormatTOML, ok: true},
		"markdown":        {path: "CLAUDE.md", ok: false},
		"no extension":    {path: "Dockerfile", ok: false},
		"dotfile no ext":  {path: ".gitignore", ok: false},
		"nested dir json": {path: "a/b/c.json", want: FormatJSON, ok: true},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, ok := FormatOf(c.path)
			if ok != c.ok {
				t.Fatalf("FormatOf(%q) ok = %v, want %v", c.path, ok, c.ok)
			}
			if got != c.want {
				t.Errorf("FormatOf(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestMerge_JSON(t *testing.T) {
	cases := map[string]struct {
		existing string
		patch    map[string]any
		want     string
	}{
		"adds a key, leaves siblings alone": {
			existing: `{"model":"opus","permissions":{"allow":["Bash"]}}`,
			patch:    map[string]any{"hooks": map[string]any{"PostToolUse": []any{"x"}}},
			want:     "{\n  \"hooks\": {\n    \"PostToolUse\": [\n      \"x\"\n    ]\n  },\n  \"model\": \"opus\",\n  \"permissions\": {\n    \"allow\": [\n      \"Bash\"\n    ]\n  }\n}\n",
		},
		"merges objects key by key": {
			existing: `{"hooks":{"PreToolUse":["keep"]}}`,
			patch:    map[string]any{"hooks": map[string]any{"PostToolUse": []any{"add"}}},
			want:     "{\n  \"hooks\": {\n    \"PostToolUse\": [\n      \"add\"\n    ],\n    \"PreToolUse\": [\n      \"keep\"\n    ]\n  }\n}\n",
		},
		"replaces an array wholesale": {
			existing: `{"hooks":{"PostToolUse":["old","older"]}}`,
			patch:    map[string]any{"hooks": map[string]any{"PostToolUse": []any{"new"}}},
			want:     "{\n  \"hooks\": {\n    \"PostToolUse\": [\n      \"new\"\n    ]\n  }\n}\n",
		},
		"null deletes a key": {
			existing: `{"model":"opus","stale":"gone"}`,
			patch:    map[string]any{"stale": nil},
			want:     "{\n  \"model\": \"opus\"\n}\n",
		},
		"scalar replaces object": {
			existing: `{"a":{"deep":1}}`,
			patch:    map[string]any{"a": "flat"},
			want:     "{\n  \"a\": \"flat\"\n}\n",
		},
		"empty target": {
			existing: ``,
			patch:    map[string]any{"a": int64(1)},
			want:     "{\n  \"a\": 1\n}\n",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := Merge([]byte(c.existing), c.patch, FormatJSON)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, c.want)
			}
		})
	}
}

// TestMerge_idempotent is the property `zordon workspace apply` depends on:
// applying the same patch again must not change a single byte, so a second
// apply is a no-op rather than a growing file.
func TestMerge_idempotent(t *testing.T) {
	cases := map[string]struct {
		format   Format
		existing string
	}{
		"json": {format: FormatJSON, existing: `{"model":"opus","hooks":{"PreToolUse":["keep"]}}`},
		"yaml": {format: FormatYAML, existing: "model: opus\nhooks:\n  PreToolUse:\n    - keep\n"},
		"toml": {format: FormatTOML, existing: "model = \"opus\"\n"},
	}
	patch := map[string]any{"hooks": map[string]any{"PostToolUse": []any{"cmd"}}}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			once, err := Merge([]byte(c.existing), patch, c.format)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			twice, err := Merge(once, patch, c.format)
			if err != nil {
				t.Fatalf("Merge again: %v", err)
			}
			if string(once) != string(twice) {
				t.Errorf("second apply changed the document:\nfirst:\n%s\nsecond:\n%s", once, twice)
			}
		})
	}
}

// TestMerge_preservesIntegers guards the json.Number path in Decode: a port
// that survives a decode/encode round trip as 8080.0 breaks its consumer.
func TestMerge_preservesIntegers(t *testing.T) {
	got, err := Merge([]byte(`{"port":8080}`), map[string]any{"host": "127.0.0.1"}, FormatJSON)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !strings.Contains(string(got), `"port": 8080`) {
		t.Errorf("got %s, want port to stay an integer", got)
	}
}

func TestMerge_rejectsNonObjectTarget(t *testing.T) {
	_, err := Merge([]byte(`[1,2,3]`), map[string]any{"a": 1}, FormatJSON)
	if err == nil {
		t.Fatal("Merge succeeded, want an error for a non-object document")
	}
	if !strings.Contains(err.Error(), "top level must be an object") {
		t.Errorf("error = %v, want it to name the top-level object requirement", err)
	}
}

func TestRegion(t *testing.T) {
	cases := map[string]struct {
		existing string
		body     string
		want     string
	}{
		"appends to an empty file": {
			existing: "",
			body:     "workspaces/",
			want:     "# >>> zordon: ws\nworkspaces/\n# <<< zordon: ws\n",
		},
		"appends after existing content": {
			existing: "node_modules/\n",
			body:     "workspaces/",
			want:     "node_modules/\n# >>> zordon: ws\nworkspaces/\n# <<< zordon: ws\n",
		},
		"adds the missing trailing newline first": {
			existing: "node_modules/",
			body:     "workspaces/",
			want:     "node_modules/\n# >>> zordon: ws\nworkspaces/\n# <<< zordon: ws\n",
		},
		"replaces an existing region in place": {
			existing: "a/\n# >>> zordon: ws\nold/\n# <<< zordon: ws\nb/\n",
			body:     "new/",
			want:     "a/\n# >>> zordon: ws\nnew/\n# <<< zordon: ws\nb/\n",
		},
		"empty body keeps the markers": {
			existing: "",
			body:     "",
			want:     "# >>> zordon: ws\n# <<< zordon: ws\n",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := Region([]byte(c.existing), "ws", c.body, "#")
			if err != nil {
				t.Fatalf("Region: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, c.want)
			}
		})
	}
}

// TestRegion_idempotent is Region's half of the apply-twice contract: the
// marked span is replaced, never appended to, so repeats cannot stack.
func TestRegion_idempotent(t *testing.T) {
	once, err := Region([]byte("node_modules/\n"), "ws", "workspaces/", "#")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	twice, err := Region(once, "ws", "workspaces/", "#")
	if err != nil {
		t.Fatalf("Region again: %v", err)
	}
	if string(once) != string(twice) {
		t.Errorf("second apply changed the file:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

// TestRegion_prefixCollision pins marker matching to whole lines. A region
// named "ignore" must not latch onto "ignore-extra"'s markers, which an
// unanchored substring search does whenever the longer name is written first:
// the sibling region is swallowed and its tail ("-extra") is left orphaned in
// the file.
func TestRegion_prefixCollision(t *testing.T) {
	doc, err := Region(nil, "ignore-extra", "b/", "#")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	doc, err = Region(doc, "ignore", "a/", "#")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}

	want := "# >>> zordon: ignore-extra\nb/\n# <<< zordon: ignore-extra\n" +
		"# >>> zordon: ignore\na/\n# <<< zordon: ignore\n"
	if string(doc) != want {
		t.Fatalf("writing \"ignore\" damaged the \"ignore-extra\" region:\ngot:\n%s\nwant:\n%s", doc, want)
	}

	again, err := Region(doc, "ignore", "a/", "#")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	if string(again) != want {
		t.Errorf("re-applying \"ignore\" changed the file:\ngot:\n%s\nwant:\n%s", again, want)
	}
}

// TestRegion_markerIsWholeLine guards the other half of anchoring: a line that
// merely contains the marker text must not be mistaken for the marker.
func TestRegion_markerIsWholeLine(t *testing.T) {
	existing := "see '# >>> zordon: ws' in the docs\n"
	doc, err := Region([]byte(existing), "ws", "x", "#")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	want := existing + "# >>> zordon: ws\nx\n# <<< zordon: ws\n"
	if string(doc) != want {
		t.Errorf("prose mentioning the marker was treated as one:\ngot:\n%s\nwant:\n%s", doc, want)
	}
}

func TestRegion_independentNames(t *testing.T) {
	first, err := Region(nil, "one", "a/", "#")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	both, err := Region(first, "two", "b/", "#")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	for _, want := range []string{"# >>> zordon: one", "a/", "# >>> zordon: two", "b/"} {
		if !strings.Contains(string(both), want) {
			t.Errorf("got %q, want it to contain %q", both, want)
		}
	}
}

func TestRegion_customComment(t *testing.T) {
	got, err := Region(nil, "ws", "x", "//")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	if want := "// >>> zordon: ws\nx\n// <<< zordon: ws\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRegion_rejectsMultilineMarkers: a newline anywhere in the marker makes a
// marker that can never be matched again, so the next apply appends a second
// copy instead of replacing the first. Rejecting beats writing a region that
// grows on every run.
func TestRegion_rejectsMultilineMarkers(t *testing.T) {
	cases := map[string]struct{ name, comment string }{
		"newline in the name":       {name: "a\nb", comment: "#"},
		"newline in the comment":    {name: "ws", comment: "#\n#"},
		"carriage return in name":   {name: "a\rb", comment: "#"},
		"carriage return in prefix": {name: "ws", comment: "#\r"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if _, err := Region(nil, c.name, "x", c.comment); err == nil {
				t.Fatalf("Region(name=%q, comment=%q) succeeded, want it refused", c.name, c.comment)
			}
		})
	}
}

// TestMerge_output pins what merge actually WRITES for each format, not just
// that writing it twice is stable — an encoder that dropped a key would still
// be idempotent.
func TestMerge_output(t *testing.T) {
	patch := map[string]any{"added": "yes", "nested": map[string]any{"k": int64(2)}}

	cases := map[string]struct {
		format   Format
		existing string
		want     []string
	}{
		"json": {
			format:   FormatJSON,
			existing: `{"kept":"original","nested":{"other":1}}`,
			want:     []string{`"kept": "original"`, `"added": "yes"`, `"other": 1`, `"k": 2`},
		},
		"yaml": {
			format:   FormatYAML,
			existing: "kept: original\nnested:\n  other: 1\n",
			// `added: "yes"` and not `added: yes`: bare yes is a YAML 1.1
			// boolean, so the encoder has to quote it or the value comes back
			// as true. Asserted deliberately — it is the encoder protecting a
			// string, not noise.
			want: []string{"kept: original", `added: "yes"`, "other: 1", "k: 2"},
		},
		"toml": {
			format:   FormatTOML,
			existing: "kept = \"original\"\n\n[nested]\nother = 1\n",
			want:     []string{`kept = "original"`, `added = "yes"`, "other = 1", "k = 2"},
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := Merge([]byte(c.existing), patch, c.format)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			for _, want := range c.want {
				if !strings.Contains(string(got), want) {
					t.Errorf("merged %s is missing %q:\n%s", c.format, want, got)
				}
			}
		})
	}
}

// TestMerge_dropsCommentsInYAMLAndTOML pins the documented cost of the
// fragment modes. It is a real limitation, not an accident, so it should fail
// loudly if a future encoder change makes the docs wrong in either direction.
func TestMerge_dropsCommentsInYAMLAndTOML(t *testing.T) {
	cases := map[string]struct {
		format   Format
		existing string
	}{
		"yaml": {format: FormatYAML, existing: "# a comment the user wrote\nkept: original\n"},
		"toml": {format: FormatTOML, existing: "# a comment the user wrote\nkept = \"original\"\n"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := Merge([]byte(c.existing), map[string]any{"added": "yes"}, c.format)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if strings.Contains(string(got), "a comment the user wrote") {
				t.Errorf("the comment survived — the documented caveat is now wrong:\n%s", got)
			}
			if !strings.Contains(string(got), "original") {
				t.Errorf("the value was lost along with the comment:\n%s", got)
			}
		})
	}

	// JSON has no comments to lose, which is why it is the safe format here.
	got, err := Merge([]byte(`{"kept":"original"}`), map[string]any{"added": "yes"}, FormatJSON)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !strings.Contains(string(got), `"kept": "original"`) {
		t.Errorf("json merge lost the original value:\n%s", got)
	}
}

func TestRegion_unterminated(t *testing.T) {
	_, err := Region([]byte("# >>> zordon: ws\nstuff\n"), "ws", "x", "#")
	if err == nil {
		t.Fatal("Region succeeded, want an error for a region with no closing marker")
	}
}
