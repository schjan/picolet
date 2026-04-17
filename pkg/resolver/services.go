package resolver

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

type bundleSubdir struct {
	Subdir       string
	Category     string
	AllowNesting bool
}

var bundleSubdirs = []bundleSubdir{
	{Subdir: "containers", Category: "container", AllowNesting: false},
	{Subdir: "volumes", Category: "volume", AllowNesting: false},
	{Subdir: "networks", Category: "network", AllowNesting: false},
	{Subdir: "kube", Category: "kube", AllowNesting: false},
	{Subdir: "systemd", Category: "systemd", AllowNesting: false},
	{Subdir: "secrets", Category: "secret", AllowNesting: false},
	{Subdir: "manifests", Category: "manifest", AllowNesting: true},
}

var bundleSubdirsByName = func() map[string]bundleSubdir {
	byName := make(map[string]bundleSubdir, len(bundleSubdirs))
	for _, subdir := range bundleSubdirs {
		byName[subdir.Subdir] = subdir
	}
	return byName
}()

type manifestRef struct {
	SrcPath     string
	LogicalPath string
}

type expandedBundles struct {
	Networks   []string
	Systemd    []string
	Volumes    []string
	Containers []string
	Kube       []string
	Secrets    []string
	Manifests  []manifestRef
}

func sortedUniqueStrings(values []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(values)))
}

func collisionKey(srcPath, category, logicalPath string) string {
	switch category {
	case "container", "volume", "network", "kube":
		return "quadlet/" + destFilename(srcPath)
	case "systemd":
		return "systemd/" + destFilename(srcPath)
	case "manifest":
		return "manifest/" + strings.TrimSuffix(logicalPath, ".tmpl")
	case "secret":
		filename := destFilename(srcPath)
		return "secret/" + strings.TrimSuffix(filename, path.Ext(filename))
	default:
		return category + "/" + destFilename(srcPath)
	}
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

	expanded.Networks = sortedUniqueStrings(expanded.Networks)
	expanded.Systemd = sortedUniqueStrings(expanded.Systemd)
	expanded.Volumes = sortedUniqueStrings(expanded.Volumes)
	expanded.Containers = sortedUniqueStrings(expanded.Containers)
	expanded.Kube = sortedUniqueStrings(expanded.Kube)
	expanded.Secrets = sortedUniqueStrings(expanded.Secrets)
	slices.SortFunc(expanded.Manifests, func(a, b manifestRef) int {
		if diff := cmp.Compare(a.LogicalPath, b.LogicalPath); diff != 0 {
			return diff
		}
		return cmp.Compare(a.SrcPath, b.SrcPath)
	})

	return expanded, errors.Join(errs...)
}

func expandServiceBundle(fsys fs.FS, service string) (*expandedBundles, error) {
	root := path.Join("services", service)
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

	rootEntries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", root, err)
	}

	bundle := &expandedBundles{}
	validSubdirs := make(map[string]bundleSubdir, len(bundleSubdirs))
	var errs []error

	for _, entry := range rootEntries {
		subdir, ok := bundleSubdirsByName[entry.Name()]
		if !ok {
			errs = append(errs, fmt.Errorf("%s/%s: unknown entry", root, entry.Name()))
			continue
		}
		if !entry.IsDir() {
			errs = append(errs, fmt.Errorf("%s/%s: expected directory", root, entry.Name()))
			continue
		}
		validSubdirs[entry.Name()] = subdir
	}

	for _, subdir := range bundleSubdirs {
		if _, ok := validSubdirs[subdir.Subdir]; !ok {
			continue
		}
		if err := bundle.readSubdir(fsys, service, subdir); err != nil {
			errs = append(errs, err)
		}
	}

	if bundle.fileCount() == 0 {
		errs = append(errs, fmt.Errorf("%s: empty service bundle", root))
	}

	errs = append(errs, bundle.validateConflicts()...)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return bundle, nil
}

func (b *expandedBundles) append(other *expandedBundles) {
	b.Networks = append(b.Networks, other.Networks...)
	b.Systemd = append(b.Systemd, other.Systemd...)
	b.Volumes = append(b.Volumes, other.Volumes...)
	b.Containers = append(b.Containers, other.Containers...)
	b.Kube = append(b.Kube, other.Kube...)
	b.Secrets = append(b.Secrets, other.Secrets...)
	b.Manifests = append(b.Manifests, other.Manifests...)
}

func (b *expandedBundles) addPath(category, srcPath string) {
	switch category {
	case "network":
		b.Networks = append(b.Networks, srcPath)
	case "systemd":
		b.Systemd = append(b.Systemd, srcPath)
	case "volume":
		b.Volumes = append(b.Volumes, srcPath)
	case "container":
		b.Containers = append(b.Containers, srcPath)
	case "kube":
		b.Kube = append(b.Kube, srcPath)
	case "secret":
		b.Secrets = append(b.Secrets, srcPath)
	}
}

func (b *expandedBundles) fileCount() int {
	return len(b.Networks) + len(b.Systemd) + len(b.Volumes) +
		len(b.Containers) + len(b.Kube) + len(b.Secrets) + len(b.Manifests)
}

func (b *expandedBundles) validateConflicts() []error {
	collisions := make(map[string][]string)

	for _, srcPath := range b.Networks {
		collisions[collisionKey(srcPath, "network", "")] = append(collisions[collisionKey(srcPath, "network", "")], srcPath)
	}
	for _, srcPath := range b.Systemd {
		collisions[collisionKey(srcPath, "systemd", "")] = append(collisions[collisionKey(srcPath, "systemd", "")], srcPath)
	}
	for _, srcPath := range b.Volumes {
		collisions[collisionKey(srcPath, "volume", "")] = append(collisions[collisionKey(srcPath, "volume", "")], srcPath)
	}
	for _, srcPath := range b.Containers {
		collisions[collisionKey(srcPath, "container", "")] = append(collisions[collisionKey(srcPath, "container", "")], srcPath)
	}
	for _, srcPath := range b.Kube {
		collisions[collisionKey(srcPath, "kube", "")] = append(collisions[collisionKey(srcPath, "kube", "")], srcPath)
	}
	for _, srcPath := range b.Secrets {
		collisions[collisionKey(srcPath, "secret", "")] = append(collisions[collisionKey(srcPath, "secret", "")], srcPath)
	}
	for _, manifest := range b.Manifests {
		key := collisionKey(manifest.SrcPath, "manifest", manifest.LogicalPath)
		collisions[key] = append(collisions[key], manifest.SrcPath)
	}

	var errs []error
	for key, srcPaths := range collisions {
		uniquePaths := sortedUniqueStrings(srcPaths)
		if len(uniquePaths) < 2 {
			continue
		}
		errs = append(errs, fmt.Errorf("bundle conflict for %s: %s", key, strings.Join(uniquePaths, ", ")))
	}
	return errs
}

func (b *expandedBundles) readSubdir(fsys fs.FS, service string, subdir bundleSubdir) error {
	root := path.Join("services", service, subdir.Subdir)
	if subdir.AllowNesting {
		return fs.WalkDir(fsys, root, func(walkPath string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fmt.Errorf("walking %s: %w", walkPath, walkErr)
			}
			if walkPath == root {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", walkPath, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s: expected regular file", walkPath)
			}
			b.Manifests = append(b.Manifests, manifestRef{
				SrcPath:     walkPath,
				LogicalPath: stripServicePrefix(walkPath, service),
			})
			return nil
		})
	}

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
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, fmt.Errorf("stat %s: %w", entryPath, err))
			continue
		}
		if !info.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("%s: expected regular file", entryPath))
			continue
		}
		b.addPath(subdir.Category, entryPath)
	}
	return errors.Join(errs...)
}

func stripServicePrefix(filePath, service string) string {
	prefix := path.Join("services", service) + "/"
	return strings.TrimPrefix(filePath, prefix)
}
