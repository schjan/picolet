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

// fileRef is one categorized source a host deploys. The category table
// (config.SpecFor) decides everything else about it.
type fileRef struct {
	SrcPath  string
	Category config.Category
	// DataPath is the logical path a DestData file deploys under
	// ("manifests/app/deploy.yml.tmpl"): Fleet-root-relative for a `paths:`
	// entry, bundle-relative for a Service Bundle. Empty for every other
	// category, whose destination follows SrcPath.
	DataPath string
	// RelPath is DataPath below its category segment, without .tmpl
	// ("app/deploy.yml"); DestData only.
	RelPath string
}

// newFileRef builds the ref of srcPath, whose category was derived from
// logical (config.CategoryForPath).
func newFileRef(srcPath, logical string, category config.Category) fileRef {
	ref := fileRef{SrcPath: srcPath, Category: category}
	if spec, _ := config.SpecFor(category); spec.Dest == config.DestData {
		ref.DataPath = logical
		ref.RelPath = strings.TrimPrefix(deployedLogicalPath(logical), spec.Subdir+"/")
	}
	return ref
}

// uniqueFileRefs sorts refs into resolution order (category table row, then
// DataPath, then SrcPath) and drops duplicates: a file reached twice (two
// `paths:` entries, a bundle and a `paths:` entry) deploys once.
func uniqueFileRefs(refs []fileRef) []fileRef {
	row := make(map[config.Category]int)
	for i, spec := range config.Specs() {
		row[spec.Category] = i
	}
	slices.SortFunc(refs, func(a, b fileRef) int {
		return cmp.Or(
			cmp.Compare(row[a.Category], row[b.Category]),
			cmp.Compare(a.DataPath, b.DataPath),
			cmp.Compare(a.SrcPath, b.SrcPath),
		)
	})
	return slices.Compact(refs)
}

type hookRef struct {
	Service string
	SrcPath string
}

// expansion is the categorized file set of a host: every file expanded from
// `paths:` entries and Service Bundles plus the `secrets:` entries, and the
// bundles' hook metadata.
type expansion struct {
	Files []fileRef
	Hooks []hookRef
}

// validateServiceName rejects bundle names that would resolve outside
// services/<name>/ once joined to "services/" and cleaned: a name with a path
// separator, "." or ".." would address another directory (e.g. "../files")
// instead of one bundle.
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

func expandServiceBundles(fsys fs.FS, services []string) (*expansion, error) {
	expanded := &expansion{}
	var errs []error

	for _, service := range services {
		bundle, err := expandServiceBundle(fsys, service)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		expanded.append(bundle)
	}

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
func expandServiceBundle(fsys fs.FS, service string) (*expansion, error) {
	if err := validateServiceName(service); err != nil {
		return nil, err
	}
	root := path.Join("services", service)
	rootEntries, err := readBundleRoot(fsys, root)
	if err != nil {
		return nil, err
	}

	bundle := &expansion{}
	hookRefs, errs := collectBundleHookRefs(root, service, rootEntries)
	bundle.Hooks = hookRefs
	if err := bundle.expandTree(fsys, root, root+"/"); err != nil {
		errs = append(errs, err)
	}

	// Only emit "empty" when nothing else explains a bundle without files.
	if len(errs) == 0 && len(bundle.Files) == 0 {
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
	return name == "picolet.yml" || name == "picolet.yml"+config.TemplateSuffix
}

// isBundleMetadataPath reports whether srcPath is a Service Bundle's root
// metadata file, services/<name>/picolet.yml[.tmpl], the only place hook
// metadata is read. A file of that name elsewhere (files/picolet.yml, a
// nested bundle directory) is ordinary content.
func isBundleMetadataPath(srcPath string) bool {
	parts := strings.Split(srcPath, "/")
	return len(parts) == 3 && parts[0] == "services" && isHookMetadataFile(parts[2])
}

func (e *expansion) append(other *expansion) {
	e.Files = append(e.Files, other.Files...)
	e.Hooks = append(e.Hooks, other.Hooks...)
}
