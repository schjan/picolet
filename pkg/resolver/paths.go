package resolver

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
)

// expandPathEntries expands `paths:` entries — Fleet-root-relative files or
// directories — into categorized sources.
func expandPathEntries(fsys fs.FS, entries []string) (*expandedBundles, error) {
	expanded := &expandedBundles{}
	var errs []error
	for _, entry := range entries {
		// Reject ".." before Clean would fold it away ("units/../files");
		// Clean only drops "./" and a trailing slash.
		root := path.Clean(entry)
		if slices.Contains(strings.Split(entry, "/"), "..") || !fs.ValidPath(root) {
			errs = append(errs, fmt.Errorf("paths entry %q: must be relative to the Fleet root", entry))
			continue
		}
		if err := expanded.expandTree(fsys, root, ""); err != nil {
			errs = append(errs, err)
		}
	}
	return expanded, errors.Join(errs...)
}

// expandTree walks root (a file or a directory, recursively, in lexical order)
// and categorizes every regular file by its logical path: the source path with
// prefix stripped. `paths:` entries use prefix "", Service Bundles
// "services/<name>/". Bundle metadata (picolet.yml, picolet.yml.tmpl) is
// skipped at any depth.
func (b *expandedBundles) expandTree(fsys fs.FS, root, prefix string) error {
	var errs []error
	walkErr := fs.WalkDir(fsys, root, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", srcPath, err)
		}
		if d.IsDir() || isHookMetadataFile(d.Name()) {
			return nil
		}
		if !d.Type().IsRegular() {
			errs = append(errs, fmt.Errorf("%s: expected regular file", srcPath))
			return nil
		}
		if err := b.addTreeFile(srcPath, strings.TrimPrefix(srcPath, prefix)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", srcPath, err))
		}
		return nil
	})
	return errors.Join(append(errs, walkErr)...)
}

// addTreeFile records one expanded file: data categories as nested refs (their
// destination follows the logical path), everything else by source path.
func (b *expandedBundles) addTreeFile(srcPath, logical string) error {
	category, err := config.CategoryForPath(logical)
	if err != nil {
		return err
	}
	spec, _ := config.SpecFor(category)
	if spec.Dest != config.DestData {
		b.addPaths(category, srcPath)
		return nil
	}
	b.NestedRefs = append(b.NestedRefs, newDataFileRef(srcPath, logical, spec))
	return nil
}

// checkTypedCategories rejects a source that a typed list and a `paths:` entry
// select in different categories at one destination: the source-keyed
// collision check sees a single source there, so the file would be deployed
// twice. Different destinations deploy both. Typed lists alone keep their
// behavior; this only fires when a `paths:` entry is involved.
func (r *Resolver) checkTypedCategories(typed map[config.Category][]string, fromPaths *expandedBundles) error {
	derived, err := r.sourceDeployments(fromPaths)
	if err != nil {
		return err
	}
	var errs []error
	for _, spec := range config.Specs() {
		for _, src := range typed[spec.Category] {
			d, ok := derived[src]
			if !ok || d.category == spec.Category {
				continue
			}
			dest, err := r.destPath(spec, src, src)
			if err != nil {
				return fmt.Errorf("%s: %w", src, err)
			}
			if dest == d.dest {
				errs = append(errs, fmt.Errorf("%s: a typed list selects it as %s, a paths: entry as %s, both deploying to %s",
					src, spec.Category, d.category, dest))
			}
		}
	}
	return errors.Join(errs...)
}

type sourceDeployment struct {
	category config.Category
	dest     string
}

// sourceDeployments maps every expanded source to its category and destination.
func (r *Resolver) sourceDeployments(expanded *expandedBundles) (map[string]sourceDeployment, error) {
	out := make(map[string]sourceDeployment)
	for category, srcs := range expanded.Paths {
		spec, _ := config.SpecFor(category)
		for _, src := range srcs {
			dest, err := r.destPath(spec, src, src)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", src, err)
			}
			out[src] = sourceDeployment{category, dest}
		}
	}
	for _, ref := range expanded.NestedRefs {
		out[ref.SrcPath] = sourceDeployment{ref.Category, r.dataDestPath(ref.LogicalPath)}
	}
	return out, nil
}
