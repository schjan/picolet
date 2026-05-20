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
