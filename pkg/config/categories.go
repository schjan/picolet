package config

import (
	"cmp"
	"slices"
)

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
	CategoryImage     Category = "image"
	CategoryBuild     Category = "build"
	CategoryPod       Category = "pod"
	CategoryArtifact  Category = "artifact"
)

// Destination is where a category's files are deployed.
type Destination int

const (
	// DestQuadlet is the picolet-owned Quadlet directory; Podman generates the unit.
	DestQuadlet Destination = iota + 1
	// DestSystemd is the systemd unit directory; files carry PicoletMarker.
	DestSystemd
	// DestData is <data dir>/<Subdir>/<RelPath>; nested layouts are allowed.
	DestData
	// DestSecret is a Podman secret ("secret:<name>").
	DestSecret
)

// Check selects the validator applied to a category's files.
type Check int

const (
	// CheckUnsupported rejects the file: Podman knows the extension, picolet
	// does not deploy it (yet).
	CheckUnsupported Check = iota
	// CheckQuadlet converts the unit with Podman's Quadlet converter.
	CheckQuadlet
	// CheckSystemd checks a raw systemd unit structurally.
	CheckSystemd
	// CheckManifest strictly unmarshals Kubernetes manifests.
	CheckManifest
	// CheckSecret rejects empty secrets and syntax-checks YAML sources.
	CheckSecret
	// CheckFile syntax-checks YAML sources and treats anything else as opaque.
	CheckFile
)

// UnitNaming is the rule deriving the systemd unit a file produces.
type UnitNaming int

const (
	// NoUnit: the file backs no systemd unit.
	NoUnit UnitNaming = iota
	// GeneratedUnit: Podman generates <name><type suffix>.service, honoring
	// ServiceName= (quadlet.GetUnitServiceName).
	GeneratedUnit
	// FileUnit: the filename (minus .tmpl) is the unit name.
	FileUnit
)

// HealthClass is how the health loop treats a category's unit.
type HealthClass int

const (
	// HealthNone: no unit to check.
	HealthNone HealthClass = iota
	// HealthDaemon: long-running; a failed unit is restarted (subject to the
	// cooldown and the externally-activated one-shot exemption).
	HealthDaemon
	// HealthReportOnly: a one-shot job its consumers pull in (Requires=); its
	// status is reported, but a failed unit is never restarted by the health
	// loop — re-running it belongs to whatever activates it.
	HealthReportOnly
)

// RestartPolicy is what apply does with the unit behind a created or updated file.
type RestartPolicy int

const (
	// RestartNone: apply never restarts the unit; changes reach services
	// through hooks, or when a consumer next starts it.
	RestartNone RestartPolicy = iota
	// RestartChanged: restart the generated service after daemon-reload
	// (timer-triggered one-shots are gated).
	RestartChanged
	// RestartActivate: enable/start/restart by [Install] and unit type; the
	// unit is disabled before its file is removed.
	RestartActivate
)

// Spec is one row of the category table.
type Spec struct {
	Category Category
	Dest     Destination
	// Subdir names the assignment list and Service Bundle subdirectory the
	// category is selected from; for DestData it is also the directory under
	// the data dir. Empty: not selectable yet.
	Subdir string
	// PathHelper is the template function resolving a RelPath to its deployed
	// path (DestData only).
	PathHelper string
	Check      Check
	// ApplyRank orders file writes and pre-delete stops only (ascending).
	ApplyRank int
	// ConvertOrder is quadlet.SupportedExtensions verbatim; the validator's
	// pre-conversion pass runs in this order.
	ConvertOrder int
	Health       HealthClass
	Restart      RestartPolicy
	// Prefill: the Quadlet resource name is derived from the unit before any
	// conversion (Podman's generateUnitsInfoMap rule).
	Prefill bool
	// PreConvert: the validator converts these units in a pass ahead of
	// per-file validation, so the side effects of conversion (e.g. a network's
	// ResourceName) are visible to the units that reference them.
	PreConvert bool
	Unit       UnitNaming
}

// categories is the category table: adding a Quadlet or systemd unit type is
// one row here plus, for Quadlets, its converter wiring in pkg/validator, and
// for selectable ones a Subdir and a typed list in AssignmentGroup.
//
// Two orderings are kept apart on purpose:
//   - ConvertOrder mirrors quadlet.SupportedExtensions: .pod converts last
//     because ConvertPod reads ContainersToStart, which ConvertContainer fills.
//   - ApplyRank orders file writes and pre-delete stops only. Restarts are not
//     rank-ordered (alphabetical after one daemon-reload); unit start order
//     comes from the Requires=/After=/BindsTo= that Quadlet generates.
//
// Row order is resolution order (the order resolved files are produced in).
var categories = []Spec{
	{
		Category: CategoryNetwork, Dest: DestQuadlet, Subdir: "networks", Check: CheckQuadlet,
		ApplyRank: 10, ConvertOrder: 2, Health: HealthDaemon, Restart: RestartChanged, PreConvert: true, Unit: GeneratedUnit,
	},
	{
		Category: CategorySystemd, Dest: DestSystemd, Subdir: "systemd", Check: CheckSystemd,
		ApplyRank: 60, Health: HealthDaemon, Restart: RestartActivate, Unit: FileUnit,
	},
	{
		Category: CategoryVolume, Dest: DestQuadlet, Subdir: "volumes", Check: CheckQuadlet,
		ApplyRank: 20, ConvertOrder: 2, Health: HealthDaemon, Restart: RestartChanged, PreConvert: true, Unit: GeneratedUnit,
	},
	{
		Category: CategoryImage, Dest: DestQuadlet, Subdir: "images", Check: CheckQuadlet,
		ApplyRank: 30, ConvertOrder: 1, Health: HealthReportOnly, Restart: RestartChanged, PreConvert: true, Unit: GeneratedUnit,
	},
	{
		// RestartNone until #127 adds rebuild-on-input-change.
		Category: CategoryBuild, Dest: DestQuadlet, Subdir: "builds", Check: CheckQuadlet,
		ApplyRank: 40, ConvertOrder: 3, Health: HealthReportOnly, Restart: RestartNone, Prefill: true, Unit: GeneratedUnit,
	},
	{
		Category: CategoryContainer, Dest: DestQuadlet, Subdir: "containers", Check: CheckQuadlet,
		ApplyRank: 100, ConvertOrder: 4, Health: HealthDaemon, Restart: RestartChanged, Prefill: true, PreConvert: true, Unit: GeneratedUnit,
	},
	{
		Category: CategoryKube, Dest: DestQuadlet, Subdir: "kube", Check: CheckQuadlet,
		ApplyRank: 110, ConvertOrder: 4, Health: HealthDaemon, Restart: RestartChanged, PreConvert: true, Unit: GeneratedUnit,
	},
	{
		Category: CategoryPod, Dest: DestQuadlet, Subdir: "pods", Check: CheckQuadlet,
		ApplyRank: 90, ConvertOrder: 5, Health: HealthDaemon, Restart: RestartChanged, Prefill: true, PreConvert: true, Unit: GeneratedUnit,
	},
	{
		Category: CategoryManifest, Dest: DestData, Subdir: "manifests", PathHelper: "manifestPath", Check: CheckManifest,
		ApplyRank: 70,
	},
	{
		Category: CategoryFile, Dest: DestData, Subdir: "files", PathHelper: "filePath", Check: CheckFile,
		ApplyRank: 80,
	},
	{
		Category: CategorySecret, Dest: DestSecret, Subdir: "secrets", Check: CheckSecret,
		ApplyRank: 50,
	},
	// Known to Podman, not deployable yet: not selectable, rejected by the validator.
	{
		Category: CategoryArtifact, Dest: DestQuadlet, Check: CheckUnsupported,
		ApplyRank: 35, ConvertOrder: 1, Health: HealthDaemon, Restart: RestartChanged, Unit: GeneratedUnit,
	},
}

// extensions maps Quadlet and systemd unit file extensions to their category.
// Manifests, files and secrets have no extension of their own (all .yml) and
// are selected by list or path instead.
var extensions = map[string]Category{
	".network":   CategoryNetwork,
	".volume":    CategoryVolume,
	".container": CategoryContainer,
	".kube":      CategoryKube,
	".image":     CategoryImage,
	".artifact":  CategoryArtifact,
	".build":     CategoryBuild,
	".pod":       CategoryPod,
	".service":   CategorySystemd,
	".timer":     CategorySystemd,
	".socket":    CategorySystemd,
	".target":    CategorySystemd,
	".path":      CategorySystemd,
}

var specsByCategory = func() map[Category]Spec {
	m := make(map[Category]Spec, len(categories))
	for _, s := range categories {
		m[s.Category] = s
	}
	return m
}()

// Specs returns the category table in resolution order.
func Specs() []Spec {
	return slices.Clone(categories)
}

// SpecFor returns the table row for c.
func SpecFor(c Category) (Spec, bool) {
	s, ok := specsByCategory[c]
	return s, ok
}

// CategoryForExtension returns the category of a unit file extension (".pod").
func CategoryForExtension(ext string) (Category, bool) {
	c, ok := extensions[ext]
	return c, ok
}

// ApplyOrder returns every category ordered by ApplyRank.
func ApplyOrder() []Category {
	sorted := Specs()
	slices.SortFunc(sorted, func(a, b Spec) int { return cmp.Compare(a.ApplyRank, b.ApplyRank) })
	out := make([]Category, len(sorted))
	for i, s := range sorted {
		out[i] = s.Category
	}
	return out
}

// Deployable returns the categories picolet deploys (every row whose Check is
// not CheckUnsupported), in resolution order.
func Deployable() []Category {
	var out []Category
	for _, s := range categories {
		if s.Check != CheckUnsupported {
			out = append(out, s.Category)
		}
	}
	return out
}

// UsesRelPath reports whether ResolvedFiles of this category carry a RelPath
// — the path relative to the category's data subdirectory, used by hooks and
// rendered deployment paths. These categories also allow nested layouts.
func (c Category) UsesRelPath() bool {
	s, ok := SpecFor(c)
	return ok && s.Dest == DestData
}
