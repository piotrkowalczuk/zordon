package alphasfile

import (
	"fmt"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// AddRequire pins repo to ref for the Alphasfile at afPath: in the zordon.mod
// above it, or, without one, in the file itself, at its top level or inside
// its package block. An existing require of repo moves to ref. It returns
// the file it wrote.
func AddRequire(afPath, repo, ref string) (string, error) {
	r, sub, inline, err := source.SplitIdentity(repo)
	if err != nil {
		return "", err
	}
	if sub != "" || inline != "" || r != repo {
		return "", fmt.Errorf("%s: name the repository alone, such as %s", repo, r)
	}
	if ref == "" {
		return "", fmt.Errorf("%s: name a branch, tag or commit, such as %s@main", repo, repo)
	}
	abs, err := filepath.Abs(afPath)
	if err != nil {
		return "", err
	}
	src := &identitySource{mods: map[string]*modFile{}}
	mod, err := src.modAbove(filepath.Dir(abs), "")
	if err != nil {
		return "", err
	}
	target := abs
	if mod != nil {
		target = mod.path
	}
	b, err := zfs.Read(target)
	if err != nil {
		return "", err
	}
	f, diags := hclwrite.ParseConfig(b, target, hcl.InitialPos)
	if diags.HasErrors() {
		return "", fmt.Errorf("%s: %s", target, diags.Error())
	}
	body := f.Body()
	if mod == nil {
		for _, blk := range body.Blocks() {
			if blk.Type() == "package" {
				body = blk.Body()
				break
			}
		}
	}
	blk := body.FirstMatchingBlock("require", []string{repo})
	if blk == nil {
		if len(body.Attributes())+len(body.Blocks()) > 0 {
			body.AppendNewline()
		}
		blk = body.AppendNewBlock("require", []string{repo})
	}
	blk.Body().SetAttributeValue("ref", cty.StringVal(ref))
	if err := zfs.AtomicWrite(target, hclwrite.Format(f.Bytes())); err != nil {
		return "", err
	}
	return target, nil
}
