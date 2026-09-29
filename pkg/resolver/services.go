package resolver

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
)

type bundleFileRef struct {
	SrcPath     string
	LogicalPath string
	Category    config.Category
	RelPath     string // deployed logical path with the category segment (manifests/, files/) stripped if present
}

type hookRef struct {
	Service string
	SrcPath string
}

type expandedBundles struct {
	Paths      map[config.Category][]string // non-data category sources, by source path
	NestedRefs []bundleFileRef              // data-category (manifest, file) refs
	Hooks      []hookRef
}

// sortedUnique returns a sorted copy with duplicates removed.
func sortedUnique(values []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(values)))
}

// validateServiceName rejects bundle names that would resolve outside the
// services/ namespace once joined to "services/" and cleaned. The repo FS is
// already DirFS-scoped so there's no arbitrary-path read, but a name like
// "../quadlets" would silently reroute the bundle root to the legacy quadlet
// directory, contradicting the documented layout.
func validateServiceName(service string) error {
	switch {
	case service == "":
		return errors.New("service name must not be empty")
	case service == "." || service == "..":
		return fmt.Errorf("service name %q is reserved", service)
	case strings.ContainsAny(service, `/\`):
		return fmt.Errorf("service name %q must not contain path separators", service)
	}
	return nil
}

func expandServiceBundles(fsys fs.FS, services []string) (*expandedBundles, error) {
	expanded := &expandedBundles{}
	var errs []error

	for _, service := range services {
		bundle, err := expandServiceBundle(fsys, service)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		expanded.append(bundle)
	}

	for category, paths := range expanded.Paths {
		expanded.Paths[category] = sortedUnique(paths)
	}
	slices.SortFunc(expanded.NestedRefs, func(a, b bundleFileRef) int {
		if diff := cmp.Compare(a.LogicalPath, b.LogicalPath); diff != 0 {
			return diff
		}
		return cmp.Compare(a.SrcPath, b.SrcPath)
	})
	slices.SortFunc(expanded.Hooks, func(a, b hookRef) int {
		if diff := cmp.Compare(a.Service, b.Service); diff != 0 {
			return diff
		}
		return cmp.Compare(a.SrcPath, b.SrcPath)
	})

	return expanded, errors.Join(errs...)
}

// expandServiceBundle expands services/<service>/ like a `paths:` directory
// (expandTree), with the logical path taken relative to the bundle, plus its
// optional root picolet.yml hook metadata.
func expandServiceBundle(fsys fs.FS, service string) (*expandedBundles, error) {
	if err := validateServiceName(service); err != nil {
		return nil, err
	}
	root := path.Join("services", service)
	rootEntries, err := readBundleRoot(fsys, root)
	if err != nil {
		return nil, err
	}

	bundle := &expandedBundles{}
	hookRefs, errs := collectBundleHookRefs(root, service, rootEntries)
	bundle.Hooks = hookRefs
	if err := bundle.expandTree(fsys, root, root+"/"); err != nil {
		errs = append(errs, err)
	}

	// Only emit "empty" when nothing else explains a bundle without files.
	if len(errs) == 0 && bundle.fileCount() == 0 {
		errs = append(errs, fmt.Errorf("%s: empty service bundle", root))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return bundle, nil
}

func readBundleRoot(fsys fs.FS, root string) ([]fs.DirEntry, error) {
	info, err := fs.Stat(fsys, root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: missing service bundle: %w", root, err)
		}
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: expected directory", root)
	}

	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", root, err)
	}
	return entries, nil
}

func collectBundleHookRefs(root, service string, entries []fs.DirEntry) ([]hookRef, []error) {
	var (
		refs []hookRef
		errs []error
	)
	for _, entry := range entries {
		if !isHookMetadataFile(entry.Name()) {
			continue
		}
		path := path.Join(root, entry.Name())
		if !entry.Type().IsRegular() {
			errs = append(errs, fmt.Errorf("%s: expected regular file", path))
			continue
		}
		refs = append(refs, hookRef{Service: service, SrcPath: path})
	}
	if len(refs) > 1 {
		return nil, []error{fmt.Errorf("%s: cannot define both picolet.yml and picolet.yml.tmpl", root)}
	}
	return refs, errs
}

func isHookMetadataFile(name string) bool {
	return name == "picolet.yml" || name == "picolet.yml.tmpl"
}

func (b *expandedBundles) append(other *expandedBundles) {
	for category, paths := range other.Paths {
		b.addPaths(category, paths...)
	}
	b.NestedRefs = append(b.NestedRefs, other.NestedRefs...)
	b.Hooks = append(b.Hooks, other.Hooks...)
}

func (b *expandedBundles) addPaths(category config.Category, srcPaths ...string) {
	if b.Paths == nil {
		b.Paths = make(map[config.Category][]string)
	}
	b.Paths[category] = append(b.Paths[category], srcPaths...)
}

func (b *expandedBundles) fileCount() int {
	n := len(b.NestedRefs)
	for _, paths := range b.Paths {
		n += len(paths)
	}
	return n
}

// stripSubdirPrefix strips the leading category segment from a logical path
// (e.g. "manifests/app/foo.yml" -> "app/foo.yml" for subdir "manifests").
// Typed-list sources may not start with the segment; in that case the input
// is returned unchanged.
func stripSubdirPrefix(logical, subdir string) string {
	if rel, ok := strings.CutPrefix(logical, subdir+"/"); ok {
		return rel
	}
	return logical
}
