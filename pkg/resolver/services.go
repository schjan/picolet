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

type bundleSubdir struct {
	Subdir   string
	Category config.Category
}

func (b bundleSubdir) AllowNesting() bool { return b.Category.UsesRelPath() }

// bundleSubdirsByName maps each selectable category's Service Bundle
// subdirectory to its category.
var bundleSubdirsByName = func() map[string]bundleSubdir {
	byName := make(map[string]bundleSubdir)
	for _, spec := range config.Specs() {
		if spec.Subdir != "" {
			byName[spec.Subdir] = bundleSubdir{Subdir: spec.Subdir, Category: spec.Category}
		}
	}
	return byName
}()

type bundleFileRef struct {
	SrcPath     string
	LogicalPath string
	Category    config.Category
	RelPath     string // deployed logical path with the bundle subdir prefix stripped if present
}

type hookRef struct {
	Service string
	SrcPath string
}

type expandedBundles struct {
	Paths      map[config.Category][]string // flat (non-data) category sources
	NestedRefs []bundleFileRef
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
	var errs []error

	validSubdirs, rootErrs := collectBundleSubdirs(root, rootEntries)
	errs = append(errs, rootErrs...)
	hookRefs, hookErrs := collectBundleHookRefs(root, service, rootEntries)
	errs = append(errs, hookErrs...)
	bundle.Hooks = append(bundle.Hooks, hookRefs...)

	for _, subdir := range validSubdirs {
		if err := bundle.readSubdir(fsys, service, subdir); err != nil {
			errs = append(errs, err)
		}
	}

	// Only emit "empty" when the root itself is clean; root-level errors already
	// explain why the bundle has no usable entries.
	if bundle.fileCount() == 0 && len(rootErrs) == 0 {
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

func collectBundleSubdirs(root string, entries []fs.DirEntry) ([]bundleSubdir, []error) {
	var (
		valid []bundleSubdir
		errs  []error
	)

	for _, entry := range entries {
		// collectBundleHookRefs owns validation/error messages for hook metadata
		// names (including the directory case). Skip them here unconditionally so
		// a directory accidentally named picolet.yml does not also trigger an
		// "unknown entry" error.
		if isHookMetadataFile(entry.Name()) {
			continue
		}
		subdir, ok := bundleSubdirsByName[entry.Name()]
		if !ok {
			errs = append(errs, fmt.Errorf("%s/%s: unknown entry", root, entry.Name()))
			continue
		}
		if !entry.IsDir() {
			errs = append(errs, fmt.Errorf("%s/%s: expected directory", root, entry.Name()))
			continue
		}
		valid = append(valid, subdir)
	}

	return valid, errs
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

// addPath records a flat bundle file; only selectable non-data categories are
// flat (data categories are nested refs).
func (b *expandedBundles) addPath(category config.Category, srcPath string) error {
	if spec, ok := config.SpecFor(category); !ok || spec.Subdir == "" || category.UsesRelPath() {
		return fmt.Errorf("resolver: unknown bundle category %q", category)
	}
	b.addPaths(category, srcPath)
	return nil
}

func (b *expandedBundles) fileCount() int {
	n := len(b.NestedRefs)
	for _, paths := range b.Paths {
		n += len(paths)
	}
	return n
}

func (b *expandedBundles) readSubdir(fsys fs.FS, service string, subdir bundleSubdir) error {
	root := path.Join("services", service, subdir.Subdir)
	if subdir.AllowNesting() {
		return b.readNestedSubdir(fsys, root, service, subdir)
	}
	return b.readFlatSubdir(fsys, root, subdir.Category)
}

func (b *expandedBundles) readNestedSubdir(fsys fs.FS, root, service string, subdir bundleSubdir) error {
	return fs.WalkDir(fsys, root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walking %s: %w", walkPath, walkErr)
		}
		if walkPath == root || d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: expected regular file", walkPath)
		}
		logical := stripServicePrefix(walkPath, service)
		b.NestedRefs = append(b.NestedRefs, bundleFileRef{
			SrcPath:     walkPath,
			LogicalPath: logical,
			Category:    subdir.Category,
			RelPath:     stripSubdirPrefix(deployedLogicalPath(logical), subdir.Subdir),
		})
		return nil
	})
}

func (b *expandedBundles) readFlatSubdir(fsys fs.FS, root string, category config.Category) error {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return fmt.Errorf("reading %s: %w", root, err)
	}
	var errs []error
	for _, entry := range entries {
		entryPath := path.Join(root, entry.Name())
		if entry.IsDir() {
			errs = append(errs, fmt.Errorf("%s: unsupported nesting", entryPath))
			continue
		}
		if !entry.Type().IsRegular() {
			errs = append(errs, fmt.Errorf("%s: expected regular file", entryPath))
			continue
		}
		if err := b.addPath(category, entryPath); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entryPath, err))
		}
	}
	return errors.Join(errs...)
}

func stripServicePrefix(filePath, service string) string {
	prefix := path.Join("services", service) + "/"
	return strings.TrimPrefix(filePath, prefix)
}

// stripSubdirPrefix strips the leading bundle subdir segment from a logical
// path (e.g. "manifests/app/foo.yml" -> "app/foo.yml" for subdir "manifests").
// Legacy top-level assignment paths may not start with the subdir; in that case
// the input is returned unchanged.
func stripSubdirPrefix(logical, subdir string) string {
	if rel, ok := strings.CutPrefix(logical, subdir+"/"); ok {
		return rel
	}
	return logical
}
