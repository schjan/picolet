package resolver

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/schjan/picolet/pkg/config"
)

// expandPathEntries expands `paths:` entries — Fleet-root-relative files or
// directories — into categorized sources.
func expandPathEntries(fsys fs.FS, entries []string) (*expandedBundles, error) {
	expanded := &expandedBundles{}
	var errs []error
	for _, entry := range entries {
		root := path.Clean(entry)
		if !fs.ValidPath(root) {
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
// prefix stripped. `paths:` entries use prefix ""; #147 moves Service Bundles
// onto this function with prefix "services/<name>/" (bundles still expand
// through readSubdir today). Bundle metadata (picolet.yml, picolet.yml.tmpl)
// is skipped at any depth.
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

// checkTypedCategories rejects a source that a typed list and `paths:` put in
// different categories: both would deploy it to one destination, and the
// source-keyed collision check sees a single source. Typed lists alone keep
// their behavior; this only fires when a `paths:` entry is involved.
func (b *expandedBundles) checkTypedCategories(typed map[config.Category][]string) error {
	derived := make(map[string]config.Category)
	for category, srcs := range b.Paths {
		for _, src := range srcs {
			derived[src] = category
		}
	}
	for _, ref := range b.NestedRefs {
		derived[ref.SrcPath] = ref.Category
	}
	var errs []error
	for _, spec := range config.Specs() {
		for _, src := range typed[spec.Category] {
			if c, ok := derived[src]; ok && c != spec.Category {
				errs = append(errs, fmt.Errorf("%s: a typed list selects it as %s, a paths: entry as %s", src, spec.Category, c))
			}
		}
	}
	return errors.Join(errs...)
}
