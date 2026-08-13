// Package zdoc reads and writes the structured configuration documents
// zordon generates into a workspace — JSON, YAML and TOML.
//
// It owns two write modes that differ in who owns the file. Encode replaces a
// whole document zordon authored. Merge and Region change a fragment of a
// document somebody else owns (a committed .claude/settings.json, a
// .gitignore), leaving everything around that fragment untouched. Both are
// idempotent: re-applying an unchanged manifest yields byte-identical output,
// which is what lets `zordon workspace apply` run as often as it likes.
//
// Caveat for the fragment modes: YAML and TOML round-trip through Go
// libraries that do not preserve comments or original formatting, so merging
// into a hand-written file of those formats rewrites it. JSON has no comments
// to lose.
package zdoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatYAML Format = "yaml"
	FormatTOML Format = "toml"
)

// FormatOf maps a file path to the format its extension implies. ok is false
// for an extension with no structured reader, which callers turn into an
// error naming the formats that do work.
func FormatOf(path string) (f Format, ok bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return FormatJSON, true
	case ".yaml", ".yml":
		return FormatYAML, true
	case ".toml":
		return FormatTOML, true
	}
	return "", false
}

// Formats lists every supported format, for error messages.
func Formats() []string { return []string{string(FormatJSON), string(FormatYAML), string(FormatTOML)} }

// Encode serializes a plain Go tree. Object keys are sorted by every encoder,
// so the output is a pure function of the input.
func Encode(v any, f Format) ([]byte, error) {
	switch f {
	case FormatJSON:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	case FormatYAML:
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case FormatTOML:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("top level must be an object, got %T", v)
		}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(m); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("unknown format %q", f)
}

// Decode parses a document into a plain Go tree. Empty input decodes to an
// empty object so a merge into a file that does not exist yet behaves like a
// merge into an empty one.
func Decode(b []byte, f Format) (map[string]any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var v any
	switch f {
	case FormatJSON:
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber() // keeps 8080 an integer instead of float64
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		v = normalizeJSON(v)
	case FormatYAML:
		if err := yaml.Unmarshal(b, &v); err != nil {
			return nil, err
		}
	case FormatTOML:
		if err := toml.Unmarshal(b, &v); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown format %q", f)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top level must be an object, got %T", v)
	}
	return m, nil
}

// Merge applies patch to existing as an RFC 7386 JSON Merge Patch and
// re-encodes the result: objects merge key by key, any other value (a scalar,
// a whole array) replaces what was there, and a null deletes its key.
//
// Array replacement is what makes this idempotent, and it is the trade: a
// patch cannot append one entry to a list the user also writes to — it owns
// the list or it does not touch it.
func Merge(existing []byte, patch any, f Format) ([]byte, error) {
	target, err := Decode(existing, f)
	if err != nil {
		return nil, err
	}
	p, ok := patch.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("patch must be an object, got %T", patch)
	}
	return Encode(mergePatch(target, p), f)
}

// Region replaces the block delimited by zordon's markers for name, appending
// it when absent. This is the text-file counterpart of Merge: it owns a
// marked span and nothing else, which is what keeps repeated applies from
// stacking copies of the same lines.
func Region(existing []byte, name, body, comment string) ([]byte, error) {
	if comment == "" {
		comment = "#"
	}
	if strings.ContainsAny(name, "\r\n") {
		return nil, fmt.Errorf("region name %q contains a newline", name)
	}
	if strings.ContainsAny(comment, "\r\n") {
		return nil, fmt.Errorf("region comment prefix %q contains a newline", comment)
	}
	openMark := fmt.Sprintf("%s >>> zordon: %s", comment, name)
	closeMark := fmt.Sprintf("%s <<< zordon: %s", comment, name)

	block := openMark + "\n"
	if body = strings.Trim(body, "\n"); body != "" {
		block += body + "\n"
	}
	block += closeMark + "\n"

	// Markers match whole LINES, never substrings. An unanchored search would
	// let region "ignore" latch onto "ignore-extra"'s markers and swallow that
	// region whole, and would mistake prose quoting a marker for the real one.
	lines := strings.Split(string(existing), "\n")
	start, end := -1, -1
	for i := range lines {
		switch strings.TrimRight(lines[i], "\r") {
		case openMark:
			if start < 0 {
				start = i
			}
		case closeMark:
			if start >= 0 && end < 0 {
				end = i
			}
		}
	}

	if start < 0 {
		text := string(existing)
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return []byte(text + block), nil
	}
	if end < 0 {
		return nil, fmt.Errorf("region %q has an opening marker but no %q", name, closeMark)
	}
	out := make([]string, 0, len(lines))
	out = append(out, lines[:start]...)
	out = append(out, strings.Split(strings.TrimSuffix(block, "\n"), "\n")...)
	out = append(out, lines[end+1:]...)
	return []byte(strings.Join(out, "\n")), nil
}

func mergePatch(target map[string]any, patch map[string]any) map[string]any {
	if target == nil {
		target = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		sub, ok := v.(map[string]any)
		if !ok {
			target[k] = v
			continue
		}
		prev, _ := target[k].(map[string]any)
		target[k] = mergePatch(prev, sub)
	}
	return target
}

// normalizeJSON turns the json.Number values UseNumber produces back into
// concrete Go numbers, so a re-encode writes 8080 rather than "8080".
func normalizeJSON(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeJSON(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeJSON(e)
		}
		return t
	}
	return v
}
