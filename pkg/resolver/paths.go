package resolver

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
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
		if err := expanded.expandTree(fsys, root, ""); errors.Is(err, errExplicitSOPSFile) {
			errs = append(errs, fmt.Errorf("paths entry %q: %w", entry, sopsOnlyError(root)))
		} else if err != nil {
			errs = append(errs, err)
		}
	}
	return expanded, errors.Join(errs...)
}

// expandTree walks root (a file or a directory, recursively, in lexical order)
// and categorizes every regular file by its logical path: the source path with
// prefix stripped. `paths:` entries use prefix "", Service Bundles
// "services/<name>/". A bundle's root metadata (services/<name>/picolet.yml,
// picolet.yml.tmpl) is skipped, however it was reached; a file of that name
// anywhere else is an ordinary file. SOPS files (sops.IsSOPSPath) and SOPS's
// config files (.sops.yaml, .sops.yml) are skipped wherever a walk finds them,
// so ciphertext is never deployed; a root that is itself a SOPS file returns
// errExplicitSOPSFile, since it is reachable only through a sops:// ref.
func (e *expansion) expandTree(fsys fs.FS, root, prefix string) error {
	var errs []error
	walkErr := fs.WalkDir(fsys, root, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", srcPath, err)
		}
		if d.IsDir() || isBundleMetadataPath(srcPath) {
			return nil
		}
		if srcPath == root && sops.IsSOPSPath(srcPath) {
			return errExplicitSOPSFile
		}
		if isSOPSWalkSkip(srcPath) {
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

// errExplicitSOPSFile is returned by expandTree when its root names a SOPS
// file; the caller reports it with the entry (sopsOnlyError).
var errExplicitSOPSFile = errors.New("explicit SOPS file")

// isSOPSWalkSkip reports whether a directory walk skips srcPath: a SOPS file
// or one of SOPS's config files.
func isSOPSWalkSkip(srcPath string) bool {
	return sops.IsSOPSPath(srcPath) || sops.IsConfigPath(srcPath)
}

// sopsOnlyError is the error for an explicit `paths:` or plain `secrets:`
// entry that names a SOPS file.
func sopsOnlyError(src string) error {
	return fmt.Errorf("SOPS file is reachable only through a sops:// ref (%s%s#/...)", sops.Scheme, src)
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
