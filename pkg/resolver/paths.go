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
		if isExplicitSOPSFile(fsys, root) {
			errs = append(errs, explicitSOPSError("paths", entry, root))
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
// "services/<name>/". A bundle's root metadata (services/<name>/picolet.yml,
// picolet.yml.tmpl) is skipped, however it was reached; a file of that name
// anywhere else is an ordinary file. SOPS files (sops.IsSOPSPath) and SOPS's
// config files (.sops.yaml, .sops.yml) are skipped wherever a walk finds them,
// so ciphertext is never deployed; callers reject an explicit entry naming a
// SOPS file before walking (isExplicitSOPSFile).
func (e *expansion) expandTree(fsys fs.FS, root, prefix string) error {
	var errs []error
	walkErr := fs.WalkDir(fsys, root, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", srcPath, err)
		}
		if d.IsDir() || isBundleMetadataPath(srcPath) {
			return nil
		}
		if sops.IsSOPSPath(srcPath) || sops.IsConfigPath(srcPath) {
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

// isExplicitSOPSFile reports whether a `paths:` entry root names a SOPS file
// (not a directory that happens to carry `.sops.` in its name). A root that
// cannot be stat'ed is left to the walk, which reports it.
func isExplicitSOPSFile(fsys fs.FS, root string) bool {
	if !sops.IsSOPSPath(root) {
		return false
	}
	info, err := fs.Stat(fsys, root)
	return err == nil && !info.IsDir()
}

// rejectPlainSOPSSecrets returns an error for every `secrets:` entry that is
// no Secret Reference but names a SOPS file.
func rejectPlainSOPSSecrets(secrets []string) error {
	var errs []error
	for _, src := range secrets {
		if !isProviderRef(src) && !strings.HasPrefix(src, sops.Scheme) && sops.IsSOPSPath(src) {
			errs = append(errs, explicitSOPSError("secrets", src, src))
		}
	}
	return errors.Join(errs...)
}

// explicitSOPSError is the error for an explicit `paths:` or plain `secrets:`
// entry that names a SOPS file: it is reachable only through a sops:// ref.
func explicitSOPSError(field, entry, src string) error {
	return fmt.Errorf("%s entry %q: SOPS file is reachable only through a sops:// ref (%s%s#/...)",
		field, entry, sops.Scheme, src)
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
