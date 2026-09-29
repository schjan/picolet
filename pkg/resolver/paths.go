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
func expandPathEntries(fsys fs.FS, entries []string) (*expansion, error) {
	expanded := &expansion{}
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
func (e *expansion) expandTree(fsys fs.FS, root, prefix string) error {
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
		if err := e.addTreeFile(srcPath, strings.TrimPrefix(srcPath, prefix)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", srcPath, err))
		}
		return nil
	})
	return errors.Join(append(errs, walkErr)...)
}

// addTreeFile records one expanded file, categorized by its logical path.
func (e *expansion) addTreeFile(srcPath, logical string) error {
	category, err := config.CategoryForPath(logical)
	if err != nil {
		return err
	}
	e.Files = append(e.Files, newFileRef(srcPath, logical, category))
	return nil
}
