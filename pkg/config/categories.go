package config

// Category identifies the kind of managed file Picolet resolves, validates,
// reconciles, and tracks in state.
type Category string

func (c Category) String() string {
	return string(c)
}

const (
	CategoryNetwork   Category = "network"
	CategorySystemd   Category = "systemd"
	CategoryVolume    Category = "volume"
	CategoryContainer Category = "container"
	CategoryKube      Category = "kube"
	CategoryManifest  Category = "manifest"
	CategoryFile      Category = "file"
	CategorySecret    Category = "secret"
)

// BundleSubdir returns the service-bundle subdirectory name for this category
// (e.g. "manifests" for CategoryManifest). Returns "" for categories that do
// not appear in service bundles.
func (c Category) BundleSubdir() string {
	switch c {
	case CategoryNetwork:
		return "networks"
	case CategorySystemd:
		return "systemd"
	case CategoryVolume:
		return "volumes"
	case CategoryContainer:
		return "containers"
	case CategoryKube:
		return "kube"
	case CategorySecret:
		return "secrets"
	case CategoryManifest:
		return "manifests"
	case CategoryFile:
		return "files"
	}
	return ""
}

// UsesRelPath reports whether ResolvedFiles of this category carry a RelPath
// — the path relative to the bundle subdirectory used by hooks and rendered
// deployment paths. These categories also allow nested layouts inside their
// bundle subdirectory.
func (c Category) UsesRelPath() bool {
	return c == CategoryManifest || c == CategoryFile
}
