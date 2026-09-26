package alphasfile

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

const (
	// LockFileName sits next to an entrypoint and pins every remote
	// repository its imports name to a commit.
	LockFileName = "zordon.lock"
	// ModFileName declares the identity of a directory that a zordon.work
	// search entry points at: `module = "github.com/owner/repo"`.
	ModFileName = "zordon.mod"
)

// Fetcher gets the repositories that imports name by identity.
type Fetcher interface {
	// Resolve returns the commit ref points at now, fetching as needed.
	Resolve(repo, ref string) (commit string, err error)
	// Materialize checks repo out at commit into dest, reusing a finished
	// checkout of the same commit.
	Materialize(repo, commit, dest string) error
}

// LoadOptions tunes how LoadTreeWith follows imports.
type LoadOptions struct {
	// Chain lists the Alphasfile of every federation level of the current
	// invocation. Importing one of them as a package is an error: it would
	// run in two levels at once.
	Chain []string
	// Home is the zordon home; remote checkouts live under Home/mod.
	Home string
	// Search are the directories of the zordon.work that applies, already
	// absolute. A repository they provide is read from there, not fetched.
	Search []string
	// LockPath is the lock file of the entrypoint; empty disables locking.
	LockPath string
	// Fetcher gets remote repositories; nil means remote imports resolve
	// only from the lock and the existing checkouts.
	Fetcher Fetcher
	// Update lists repositories to re-resolve to their newest commit;
	// UpdateAll re-resolves every one.
	Update    []string
	UpdateAll bool
}

// LockChange is one lock entry that loading added or moved.
type LockChange struct {
	Repo string
	Ref  string
	Old  string
	New  string
}

// resolved is where an import points: a file or a package directory on
// disk, the identity the config hash records for it, the checkout root a
// remote file's relative paths must stay inside, and where it came from.
type resolved struct {
	path     string
	identity string
	confine  string
	repoAt   string
	origin   string
}

// importSource resolves an import block to a file or a package directory.
type importSource interface {
	resolve(importer *treeFile, imp *importBlock) (resolved, error)
}

type identitySource struct {
	opts    LoadOptions
	lock    *lockFile
	refs    map[string]refUse
	mods    map[string]string
	changes []LockChange
}

type refUse struct {
	ref string
	at  hcl.Range
}

func newIdentitySource(opts LoadOptions) (*identitySource, error) {
	lock, err := readLock(opts.LockPath)
	if err != nil {
		return nil, err
	}
	return &identitySource{opts: opts, lock: lock, refs: map[string]refUse{}, mods: map[string]string{}}, nil
}

func (s *identitySource) resolve(importer *treeFile, imp *importBlock) (resolved, error) {
	if imp.Git != nil {
		return resolved{}, fmt.Errorf("%s: %s %q: remote imports (git {}) are not supported yet; name the repository in the path instead, such as github.com/owner/repo/dir@v1.0.0", imp.DefRange, imp.keyword, imp.Path)
	}
	if isLocalPath(imp.Path) {
		return s.resolveLocal(importer, imp)
	}
	repo, sub, ref, err := source.SplitIdentity(imp.Path)
	if err != nil {
		return resolved{}, fmt.Errorf("%s: %s %q is neither a local path (those start with ./, ../, / or ~/) nor a remote identity such as github.com/owner/repo/dir@v1.0.0: %w", imp.DefRange, imp.keyword, imp.Path, err)
	}
	id := strings.TrimSuffix(repo+"/"+sub, "/")
	if res, ok, err := s.search(imp, id); err != nil || ok {
		return res, err
	}
	if ref == "" {
		return resolved{}, fmt.Errorf("%s: %s %q: a remote import needs a version: add @<branch, tag or commit>, or provide %s through a search entry in zordon.work", imp.DefRange, imp.keyword, imp.Path, repo)
	}
	if prev, seen := s.refs[repo]; seen && prev.ref != ref {
		return resolved{}, fmt.Errorf("%s: %s %q asks for %s@%s, but %s asks for @%s; one stack uses one version of a repository", imp.DefRange, imp.keyword, imp.Path, repo, ref, prev.at, prev.ref)
	}
	s.refs[repo] = refUse{ref: ref, at: imp.DefRange}
	commit, err := s.commitFor(repo, ref, imp)
	if err != nil {
		return resolved{}, err
	}
	if s.opts.Home == "" {
		return resolved{}, fmt.Errorf("%s: %s %q: remote imports need a zordon home for their checkouts", imp.DefRange, imp.keyword, imp.Path)
	}
	dest := filepath.Join(s.opts.Home, "mod", filepath.FromSlash(repo)+"@"+commit)
	if s.opts.Fetcher == nil {
		if !zfs.Exists(dest) {
			return resolved{}, fmt.Errorf("%s: %s %q: %s@%s is not checked out and fetching is off", imp.DefRange, imp.keyword, imp.Path, repo, shortCommit(commit))
		}
	} else if err := s.opts.Fetcher.Materialize(repo, commit, dest); err != nil {
		return resolved{}, fmt.Errorf("%s: %s %q: %w", imp.DefRange, imp.keyword, imp.Path, err)
	}
	repoAt := repo + "@" + commit
	return resolved{
		path:     filepath.Join(dest, filepath.FromSlash(sub)),
		identity: repoAt + "//" + sub,
		confine:  dest,
		repoAt:   repoAt,
		origin:   repo + "@" + shortCommit(commit),
	}, nil
}

// resolveLocal resolves a path relative to the importing file. Inside a
// remote checkout it must stay inside that checkout, so a fetched file can
// never read the machine it runs on.
func (s *identitySource) resolveLocal(importer *treeFile, imp *importBlock) (resolved, error) {
	p := filepath.Clean(resolveSrcDir(importer.dir, imp.Path))
	if importer.confine == "" {
		return resolved{path: p, identity: p}, nil
	}
	if !strings.HasPrefix(imp.Path, "./") && !strings.HasPrefix(imp.Path, "../") {
		return resolved{}, fmt.Errorf("%s: %s %q: a file fetched from a remote repository may import relative paths only", imp.DefRange, imp.keyword, imp.Path)
	}
	if !zfs.Within(importer.confine, p) {
		return resolved{}, fmt.Errorf("%s: %s %q: leaves the checkout of %s", imp.DefRange, imp.keyword, imp.Path, importer.repoAt)
	}
	rel, err := filepath.Rel(importer.confine, p)
	if err != nil {
		return resolved{}, err
	}
	return resolved{
		path:     p,
		identity: importer.repoAt + "//" + filepath.ToSlash(rel),
		confine:  importer.confine,
		repoAt:   importer.repoAt,
		origin:   importer.origin,
	}, nil
}

// search finds id in the zordon.work search directories: a directory whose
// zordon.mod module is a prefix of id, or a root laid out as
// <host>/<owner>/<repo>. Two directories providing id is an error.
func (s *identitySource) search(imp *importBlock, id string) (resolved, bool, error) {
	type hit struct{ dir, path, how string }
	var hits []hit
	for _, dir := range s.opts.Search {
		mod, err := s.modOf(dir)
		if err != nil {
			return resolved{}, false, err
		}
		if mod != "" && (id == mod || strings.HasPrefix(id, mod+"/")) {
			rest := strings.TrimPrefix(strings.TrimPrefix(id, mod), "/")
			hits = append(hits, hit{dir: dir, path: filepath.Join(dir, filepath.FromSlash(rest)), how: fmt.Sprintf("zordon.mod: module %q", mod)})
			continue
		}
		if p := filepath.Join(dir, filepath.FromSlash(id)); zfs.Exists(p) {
			hits = append(hits, hit{dir: dir, path: p, how: "path " + p})
		}
	}
	switch len(hits) {
	case 0:
		return resolved{}, false, nil
	case 1:
		return resolved{path: hits[0].path, identity: hits[0].path, origin: "search " + hits[0].dir}, true, nil
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		lines[i] = "  " + h.dir + " (" + h.how + ")"
	}
	return resolved{}, false, fmt.Errorf("%s: %s %q: %s is provided by %d search entries in zordon.work:\n%s\nkeep one of them", imp.DefRange, imp.keyword, imp.Path, id, len(hits), strings.Join(lines, "\n"))
}

func (s *identitySource) modOf(dir string) (string, error) {
	if mod, ok := s.mods[dir]; ok {
		return mod, nil
	}
	mod, err := readMod(dir)
	if err != nil {
		return "", err
	}
	s.mods[dir] = mod
	return mod, nil
}

func (s *identitySource) commitFor(repo, ref string, imp *importBlock) (string, error) {
	e, locked := s.lock.repos[repo]
	if locked && e.ref == ref && !s.updating(repo) {
		s.lock.used[repo] = true
		return e.commit, nil
	}
	if s.opts.Fetcher == nil {
		return "", fmt.Errorf("%s: %s %q: %s@%s is not in %s and fetching is off", imp.DefRange, imp.keyword, imp.Path, repo, ref, LockFileName)
	}
	commit, err := s.opts.Fetcher.Resolve(repo, ref)
	if err != nil {
		return "", fmt.Errorf("%s: %s %q: %w", imp.DefRange, imp.keyword, imp.Path, err)
	}
	if !locked || e.commit != commit || e.ref != ref {
		s.changes = append(s.changes, LockChange{Repo: repo, Ref: ref, Old: e.commit, New: commit})
		s.lock.dirty = true
	}
	s.lock.repos[repo] = lockEntry{ref: ref, commit: commit}
	s.lock.used[repo] = true
	return commit, nil
}

func (s *identitySource) updating(repo string) bool {
	return s.opts.UpdateAll || slices.Contains(s.opts.Update, repo)
}

type lockFile struct {
	path  string
	repos map[string]lockEntry
	used  map[string]bool
	dirty bool
}

type lockEntry struct {
	ref    string
	commit string
}

type lockDoc struct {
	Repos []struct {
		Repo   string `hcl:"repo,label"`
		Ref    string `hcl:"ref"`
		Commit string `hcl:"commit"`
	} `hcl:"repo,block"`
}

func readLock(path string) (*lockFile, error) {
	l := &lockFile{path: path, repos: map[string]lockEntry{}, used: map[string]bool{}}
	if path == "" || !zfs.Exists(path) {
		return l, nil
	}
	b, err := zfs.Read(path)
	if err != nil {
		return nil, err
	}
	file, diags := hclparse.NewParser().ParseHCL(b, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", LockFileName, diags.Error())
	}
	var doc lockDoc
	if diags := gohcl.DecodeBody(file.Body, nil, &doc); diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", LockFileName, diags.Error())
	}
	for _, r := range doc.Repos {
		l.repos[r.Repo] = lockEntry{ref: r.Ref, commit: r.Commit}
	}
	return l, nil
}

// write rewrites the lock with the repositories this load used, sorted.
func (l *lockFile) write() error {
	var b strings.Builder
	b.WriteString("# Generated by zordon. Commit it; `zordon update` moves the pins.\n")
	repos := make([]string, 0, len(l.used))
	for repo := range l.used {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		e := l.repos[repo]
		fmt.Fprintf(&b, "\nrepo %q {\n  ref    = %q\n  commit = %q\n}\n", repo, e.ref, e.commit)
	}
	return zfs.AtomicWrite(l.path, []byte(b.String()))
}

type modDoc struct {
	Module string `hcl:"module"`
}

// readMod returns the module path declared by dir/zordon.mod, or "" when
// the directory has none.
func readMod(dir string) (string, error) {
	path := filepath.Join(dir, ModFileName)
	if !zfs.Exists(path) {
		return "", nil
	}
	b, err := zfs.Read(path)
	if err != nil {
		return "", err
	}
	file, diags := hclparse.NewParser().ParseHCL(b, path)
	if diags.HasErrors() {
		return "", fmt.Errorf("%s: %s", path, diags.Error())
	}
	var doc modDoc
	if diags := gohcl.DecodeBody(file.Body, nil, &doc); diags.HasErrors() {
		return "", fmt.Errorf("%s", diags.Error())
	}
	if _, _, _, err := source.SplitIdentity(doc.Module); err != nil {
		return "", fmt.Errorf("%s: module %q: %w", path, doc.Module, err)
	}
	return strings.TrimSuffix(doc.Module, "/"), nil
}

type workDoc struct {
	Search []struct {
		Dir      string    `hcl:"dir,label"`
		DefRange hcl.Range `hcl:",def_range"`
	} `hcl:"search,block"`
}

// ReadWorkFile returns the search directories of a zordon.work file,
// absolute, in declaration order. Relative directories resolve against the
// file's own directory.
func ReadWorkFile(path string) ([]string, error) {
	b, err := zfs.Read(path)
	if err != nil {
		return nil, err
	}
	file, diags := hclparse.NewParser().ParseHCL(b, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("zordon.work: %s", diags.Error())
	}
	var doc workDoc
	if diags := gohcl.DecodeBody(file.Body, nil, &doc); diags.HasErrors() {
		return nil, fmt.Errorf("zordon.work: %s", diags.Error())
	}
	seen := map[string]hcl.Range{}
	out := make([]string, 0, len(doc.Search))
	for _, s := range doc.Search {
		dir := filepath.Clean(resolveSrcDir(filepath.Dir(path), s.Dir))
		info, err := zfs.Stat(dir)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("%s: search %q: %s is not a directory", s.DefRange, s.Dir, dir)
		}
		if prev, dup := seen[dir]; dup {
			return nil, fmt.Errorf("%s: search %q repeats the entry at %s", s.DefRange, s.Dir, prev)
		}
		seen[dir] = s.DefRange
		out = append(out, dir)
	}
	return out, nil
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
