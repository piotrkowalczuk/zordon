package alphasfile

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultModule is the module of every top-level service block: the flat
// namespace an Alphasfile had before modules existed. Its ids carry no
// module prefix, so pre-module manifests keep their exact identities.
const DefaultModule = ""

// ServiceRef is the canonical id of a service: `service.<tc>.<name>` in the
// default module, `module.<m>.service.<tc>.<name>` inside a module, and
// `package.<p>.module.<m>.service.<tc>.<name>` inside a package's module,
// whose module id is "<p>/<m>". It is the barrier entity prefix, the
// fs::service::bin handle and the MCP provision id root, so every producer
// of such a string goes through here.
func ServiceRef(module, toolchain, name string) string {
	if module == DefaultModule {
		return "service." + toolchain + "." + name
	}
	return modulePath(module) + ".service." + toolchain + "." + name
}

// modulePath is the HCL path of a module id: module.<m>, or
// package.<p>.module.<m> for a package's module.
func modulePath(module string) string {
	if p, m, ok := strings.Cut(module, "/"); ok {
		return "package." + p + ".module." + m
	}
	return "module." + module
}

// parseModulePath is the inverse of modulePath on the front of id: it
// returns the module id and what follows the module path, without its dot.
func parseModulePath(id string) (module, rest string, ok bool) {
	if after, found := strings.CutPrefix(id, "package."); found {
		p, after, found := strings.Cut(after, ".module.")
		if !found || p == "" {
			return "", "", false
		}
		m, rest, found := strings.Cut(after, ".")
		if !found || m == "" {
			return "", "", false
		}
		return p + "/" + m, rest, true
	}
	if after, found := strings.CutPrefix(id, "module."); found {
		m, rest, found := strings.Cut(after, ".")
		if !found || m == "" {
			return "", "", false
		}
		return m, rest, true
	}
	return "", "", false
}

// ProvisionRef is the canonical id of one provision step of a service.
func ProvisionRef(module, toolchain, name, step string) string {
	return ServiceRef(module, toolchain, name) + ".runtime.provision." + step
}

// ParseServiceRef splits a canonical id (optionally followed by more
// segments or an `@state` suffix) back into its service coordinates. rest
// is whatever followed the service name, without its leading dot.
func ParseServiceRef(id string) (module, toolchain, name, rest string, ok bool) {
	if at := strings.LastIndexByte(id, '@'); at >= 0 {
		id = id[:at]
	}
	if m, rest, found := parseModulePath(id); found {
		after, found := strings.CutPrefix(rest, "service.")
		if !found {
			return "", "", "", "", false
		}
		module, id = m, after
	} else if after, found := strings.CutPrefix(id, "service."); found {
		id = after
	} else {
		return "", "", "", "", false
	}
	parts := strings.SplitN(id, ".", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", false
	}
	if len(parts) == 3 {
		rest = parts[2]
	}
	return module, parts[0], parts[1], rest, true
}

// DisplayName is the user-facing service name: bare in the default module,
// `<module>/<name>` inside a module. It is what picks, `zordon status`,
// checkout dirs and worktree branches use.
func DisplayName(module, name string) string {
	if module == DefaultModule {
		return name
	}
	return module + "/" + name
}

// ShortName is DisplayName without the module of a package when the module
// is named like the package: <p>/<svc> for <p>/<p>/<svc>. Package and
// module names are unique across the stack, so it names one service.
func ShortName(module, name string) string {
	if p, m, ok := strings.Cut(module, "/"); ok && p == m {
		return p + "/" + name
	}
	return DisplayName(module, name)
}

// PackageOf returns the package a module id belongs to: the part before '/'
// of <package>/<module>.
func PackageOf(module string) (string, bool) {
	p, _, ok := strings.Cut(module, "/")
	return p, ok
}

// SplitDisplayName is the inverse of DisplayName.
func SplitDisplayName(display string) (module, name string) {
	if i := strings.LastIndexByte(display, '/'); i >= 0 {
		return display[:i], display[i+1:]
	}
	return DefaultModule, display
}

// ToolchainKey names one materialized toolchain pin: `<lang>` for the
// entrypoint's pin, `<module>/<lang>` for a module's own pin.
func ToolchainKey(module, lang string) string {
	if module == DefaultModule {
		return lang
	}
	return module + "/" + lang
}

// ToolchainRef is the barrier entity of a toolchain pin, shaped like the
// HCL path that reaches it: `toolchain.<lang>` for the entrypoint's pin,
// `module.<m>.toolchain.<lang>` for a module's own pin.
func ToolchainRef(module, lang string) string {
	if module == DefaultModule {
		return "toolchain." + lang
	}
	return modulePath(module) + ".toolchain." + lang
}

// ParseToolchainRef maps a toolchain barrier entity back to its toolchain
// key. The flat form passes its key through verbatim, since a pkg tool key
// such as `aqua:etcd-io/etcd` may itself contain '/'.
func ParseToolchainRef(entity string) (key string, ok bool) {
	if m, rest, found := parseModulePath(entity); found {
		lang, found := strings.CutPrefix(rest, "toolchain.")
		if !found || lang == "" {
			return "", false
		}
		return ToolchainKey(m, lang), true
	}
	key, found := strings.CutPrefix(entity, "toolchain.")
	if !found || key == "" {
		return "", false
	}
	return key, true
}

// ToolchainLang recovers the language label from a toolchain key.
func ToolchainLang(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// annotateModules stamps every service block with the module it was
// declared in and validates identity: module names must be traversable
// HCL identifiers (they appear as `module.<name>` in expressions) declared
// once per file, and service names may not contain '/', the display-name
// separator between module and service.
func annotateModules(root *rootBlock) error {
	seen := map[string]*moduleBlock{}
	for _, mb := range root.Modules {
		if !moduleNameRe.MatchString(mb.Name) {
			return fmt.Errorf("%s: invalid module name %q: use letters, digits, '_' or '-' and start with a letter", mb.DefRange, mb.Name)
		}
		if prev, dup := seen[mb.Name]; dup {
			return fmt.Errorf("duplicate module %q: declared at %s and %s", mb.Name, prev.DefRange, mb.DefRange)
		}
		seen[mb.Name] = mb
		for _, sb := range mb.Services {
			sb.module = mb.Name
		}
	}
	for _, sb := range root.Services {
		if mb, clash := seen[sb.Name]; clash {
			return fmt.Errorf("%s: module %q has the same name as the top-level service declared at %s; both would own <state>/{bin,src,etc,var}/%s and the git branch zordon/<ws>/%s, so rename one of them", mb.DefRange, mb.Name, sb.DefRange, sb.Name, sb.Name)
		}
	}
	services := root.allServices()
	for _, pb := range root.Packages {
		if !moduleNameRe.MatchString(pb.Name) {
			return fmt.Errorf("%s: invalid package name %q: use letters, digits, '_' or '-' and start with a letter", pb.DefRange, pb.Name)
		}
		inPkg := map[string]*moduleBlock{}
		for _, mb := range pb.Modules {
			if !moduleNameRe.MatchString(mb.Name) {
				return fmt.Errorf("%s: invalid module name %q: use letters, digits, '_' or '-' and start with a letter", mb.DefRange, mb.Name)
			}
			if prev, dup := inPkg[mb.Name]; dup {
				return fmt.Errorf("duplicate module %q in package %q: declared at %s and %s", mb.Name, pb.Name, prev.DefRange, mb.DefRange)
			}
			inPkg[mb.Name] = mb
			for _, sb := range mb.Services {
				sb.module = mb.Name
			}
			services = append(services, mb.Services...)
		}
	}
	for _, sb := range services {
		if strings.Contains(sb.Name, "/") {
			return fmt.Errorf("%s: invalid service name %q: '/' separates a module from a service name; declare the service inside a module block instead", sb.DefRange, sb.Name)
		}
	}
	return nil
}

var moduleNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
