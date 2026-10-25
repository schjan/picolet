package resolver

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"go.yaml.in/yaml/v4"

	"github.com/schjan/picolet/pkg/config"
	op "github.com/schjan/picolet/pkg/onepassword"
	pp "github.com/schjan/picolet/pkg/protonpass"
	"github.com/schjan/picolet/pkg/sops"
)

// ResolvedFile represents a single rendered file with its destination path.
type ResolvedFile struct {
	// SrcPath is the source template/file path within the repo.
	SrcPath string
	// DestPath is where the file should be deployed on the target host.
	DestPath string
	// Content is the rendered file content.
	Content string
	// Category describes the file type (network, container, kube, manifest, etc.).
	Category config.Category
	// ParsedUnit is the parsed quadlet unit file; nil for non-quadlet files or on parse error.
	ParsedUnit *parser.UnitFile
	// ServiceName is the derived systemd service name (e.g. "foo.service"); "" for non-quadlets.
	ServiceName string
	// RelPath is the file's path below its data category directory
	// (e.g. "config/scrape.yml" for a manifest at manifests/config/scrape.yml).
	// Only set for manifest- and file-category resolved files.
	RelPath string
	// SopsValues is true when Content holds a value read through a sops://
	// ref: a readSopsSecret or readSopsData call during the file's render
	// (also through renderTemplate) or a direct sops:// secrets: entry.
	// Keyless, such a value is a placeholder, so the content can't be
	// compared with what the Agent applied.
	SopsValues bool
}

// ResolvedHost is the complete desired state for a single host.
type ResolvedHost struct {
	Hostname string
	Host     *config.HostConfig
	Files    []ResolvedFile
	Hooks    []config.Hook
	// DataDir is the data directory picolet writes files and manifests to
	// (their DestPath). HostDataDir is the host-visible path of it that the
	// filePath and manifestPath helpers emit (Config.HostDataDir, else DataDir).
	DataDir     string
	HostDataDir string
	// SopsFiles are the SOPS files (Fleet-relative paths, sorted) this
	// resolve's sops:// refs reached: template helpers, templated refs and
	// direct secrets: entries. validate's audience pass unions them over
	// every Host.
	SopsFiles []string
}

// Config holds configuration for creating a Resolver.
type Config struct {
	FS             fs.FS
	Config         *config.Config
	SecretReader   SecretReader
	OpSecretReader SecretRefReader
	PPSecretReader SecretRefReader
	// Sops resolves sops:// refs on the Agent (the decrypting reader, one
	// per resolve). Nil makes every sops:// ref fail the resolve; unlike
	// op:// and pass://, SOPS never renders a placeholder without a reader.
	// validate, resolve and bootstrap use NewKeyless instead.
	Sops SopsReader
	// NoSopsIdentityHint is the fix the no-identity error names when Sops
	// is nil; empty: sops.NoIdentityHint (set sops.age_identity_path).
	NoSopsIdentityHint string
	Rootless           bool
	Strict             bool

	// keylessSops (set only by NewKeyless) replaces Sops with
	// the keyless reader, built per host resolve for that Host so its
	// recipient checks see the Host's age_recipients:.
	keylessSops bool

	// QuadletDir, SystemdDir, and DataDir override the defaults computed by
	// ResolveDirs. Empty fields fall back to the default for the given
	// Rootless mode. Used by tests to isolate destination paths from a
	// shared host filesystem; production callers leave them empty.
	QuadletDir string
	SystemdDir string
	DataDir    string

	// HostDataDir is the path the filePath/manifestPath template helpers
	// emit. It does NOT change where picolet writes files (that stays DataDir).
	// Empty falls back to DataDir, so native deployments are unaffected; set it
	// when picolet runs containerized and the host sees the data dir at a
	// different path than picolet does.
	HostDataDir string

	// Logger receives the resolver's log lines (secret resolution, template
	// rendering). Nil falls back to slog.Default().
	Logger *slog.Logger
}

// Resolver renders templates and resolves the desired state for hosts.
type Resolver struct {
	fsys           fs.FS
	cfg            *config.Config
	secretReader   SecretReader
	opSecretReader SecretRefReader
	ppSecretReader SecretRefReader
	sopsReader     SopsReader
	noSopsHint     string
	keylessSops    bool
	quadletDir     string
	systemdDir     string
	dataDir        string
	hostDataDir    string
	rootless       bool
	strict         bool
	logger         *slog.Logger
	// rendering is the top-level file being rendered when SOPS values may
	// not be rendered into it (sopsGuard); nil otherwise. Set by withRender
	// on the per-resolve copy resolveHostFileSet makes.
	rendering *unitRender
	// sopsFiles records the SOPS files this resolve's refs reached; set
	// fresh on every per-resolve copy (forResolve), nil on the shared
	// Resolver.
	sopsFiles sopsFileSet
	// sopsCalls counts this resolve's readSopsSecret/readSopsData calls
	// (sopsGuard), so buildFiles can tell which files read a SOPS value.
	sopsCalls int
	// keylessReader is this resolve's keyless SOPS reader (NewKeyless),
	// built for its Host on the per-resolve copy; it serves the template
	// helpers and the direct refs alike, so each file is read once.
	keylessReader *sops.KeylessReader
}

// Rootless reports whether the resolver is configured for rootless mode.
func (r *Resolver) Rootless() bool { return r.rootless }

// NewKeyless is New with the keyless SOPS reader over rc.FS in place of
// Sops: the resolver of validate, resolve and bootstrap, where
// sops:// values render as placeholders and no key is read. Each host
// resolve checks its refs for that Host, its age_recipients: included. Use it
// rather than a hand-set reader, so no keyless call site forgets it (which
// would fail every sops:// ref).
func NewKeyless(rc Config) (*Resolver, error) {
	rc.Sops = nil
	rc.keylessSops = true
	return New(rc)
}

// New creates a new Resolver.
// Pass nil for SecretReader to use placeholder mode (validate/CI).
// When Rootless is true, destination paths use ~/.config/ and ~/.local/share/ instead of /etc/ and /var/lib/.
func New(rc Config) (*Resolver, error) {
	quadletDir, systemdDir, dataDir, err := ResolveDirs(rc.Rootless)
	if err != nil {
		return nil, err
	}
	if rc.QuadletDir != "" {
		quadletDir = rc.QuadletDir
	}
	if rc.SystemdDir != "" {
		systemdDir = rc.SystemdDir
	}
	if rc.DataDir != "" {
		dataDir = rc.DataDir
	}
	hostDataDir := dataDir
	if rc.HostDataDir != "" {
		hostDataDir = rc.HostDataDir
	}
	if err := checkHostDataDir(hostDataDir); err != nil {
		return nil, err
	}
	return &Resolver{
		fsys:           rc.FS,
		cfg:            rc.Config,
		secretReader:   rc.SecretReader,
		opSecretReader: rc.OpSecretReader,
		ppSecretReader: rc.PPSecretReader,
		sopsReader:     rc.Sops,
		noSopsHint:     cmp.Or(rc.NoSopsIdentityHint, sops.NoIdentityHint),
		keylessSops:    rc.keylessSops,
		quadletDir:     quadletDir,
		systemdDir:     systemdDir,
		dataDir:        dataDir,
		hostDataDir:    hostDataDir,
		rootless:       rc.Rootless,
		strict:         rc.Strict,
		logger:         cmp.Or(rc.Logger, slog.Default()),
	}, nil
}

// checkHostDataDir enforces the contract that lets Fleet templates emit
// filePath/manifestPath unquoted into Quadlet, systemd and shell lines: an
// absolute path of letters, digits and . _ - + @ / only. Anything else
// (whitespace, quotes, $ and backticks, systemd's %, Volume='s :) would be
// split or expanded by one of those parsers.
func checkHostDataDir(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("host data dir %q must be an absolute path", dir)
	}
	for _, r := range dir {
		if !isPlainPathRune(r) {
			return fmt.Errorf("host data dir %q: only letters, digits and . _ - + @ / are allowed; "+
				"filePath and manifestPath emit it unquoted into units and scripts", dir)
		}
	}
	return nil
}

func isPlainPathRune(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return true
	default:
		return strings.ContainsRune("._-+@/", r)
	}
}

// ResolveDirs computes destination directories based on rootless mode.
// Quadlet files are placed in a picolet-owned subdirectory so that orphan
// detection can safely scan and remove any file in that directory.
func ResolveDirs(rootless bool) (quadletDir, systemdDir, dataDir string, err error) {
	if !rootless {
		return "/etc/containers/systemd/picolet", "/etc/systemd/system", "/var/lib/picolet", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("getting home directory: %w", err)
	}
	return filepath.Join(home, ".config", "containers", "systemd", "picolet"),
		filepath.Join(home, ".config", "systemd", "user"),
		filepath.Join(home, ".local", "share", "picolet"), nil
}

// ResolveHost computes the complete desired state for a given hostname.
func (r *Resolver) ResolveHost(ctx context.Context, hostname string) (*ResolvedHost, error) {
	host, ok := r.cfg.FindHost(hostname)
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}
	r.cfg.Assignments.WarnUnassigned(host, r.logger)
	return r.resolveHostFileSet(ctx, hostname, host, r.cfg.Assignments.Resolve(host), nil)
}

// ResolveServicesForHost resolves only the listed service bundles for a host.
// Host assignment metadata remains the same as a full host resolve, but only
// files and hooks from the requested service bundles are rendered.
func (r *Resolver) ResolveServicesForHost(ctx context.Context, hostname string, services []string) (*ResolvedHost, error) {
	host, ok := r.cfg.FindHost(hostname)
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}
	r.cfg.Assignments.WarnUnassigned(host, r.logger)
	full := r.cfg.Assignments.Resolve(host)
	available := config.SortedUnique(full.Services)
	requested := config.SortedUnique(services)
	var missing []string
	for _, service := range requested {
		if !slices.Contains(available, service) {
			missing = append(missing, service)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("service(s) not assigned to host %s: %s (available: %s)",
			hostname, strings.Join(missing, ", "), strings.Join(available, ", "))
	}
	return r.resolveHostFileSet(ctx, hostname, host, &config.ResolvedFileSet{Services: requested}, serviceTemplatePrefixes(requested))
}

func (r *Resolver) resolveHostFileSet(ctx context.Context, hostname string, host *config.HostConfig, fileSet *config.ResolvedFileSet, templatePrefixes []string) (*ResolvedHost, error) {
	return r.forResolve().resolveHostFileSetOnCopy(ctx, hostname, host, fileSet, templatePrefixes)
}

// forResolve returns the Resolver copy one resolve renders on: rendering is
// per-resolve state, and one Resolver may resolve Hosts concurrently
// (validate). The copy is shallow; every other field is read-only during a
// resolve, so a new mutable field must be copied here too.
func (r *Resolver) forResolve() *Resolver {
	rr := *r
	rr.rendering = nil
	rr.sopsFiles = sopsFileSet{}
	rr.sopsCalls = 0
	rr.keylessReader = nil
	return &rr
}

func (r *Resolver) resolveHostFileSetOnCopy(ctx context.Context, hostname string, host *config.HostConfig, fileSet *config.ResolvedFileSet, templatePrefixes []string) (*ResolvedHost, error) {
	if r.keylessSops {
		r.keylessReader = keylessSopsReader(r.fsys, host)
	}
	tmplData, err := NewTemplateData(r.cfg, hostname)
	if err != nil {
		return nil, err
	}

	providers := []ProviderTemplate{
		r.provider(OpProvider(r.opSecretReader)),
		r.provider(PPProvider(r.ppSecretReader)),
		r.provider(r.guardedSopsProvider()),
	}
	// hostDataDir (not dataDir) drives filePath/manifestPath: those helpers emit
	// path strings baked into rendered quadlets, which the host's podman must
	// resolve. dataDir remains the write path (dataDestPath).
	registry, caches, err := buildRegistry(ctx, r.fsys, r.secretReader, providers, r.hostDataDir, prefixFilter(templatePrefixes), siblings(tmplData), r.logger)
	if err != nil {
		return nil, fmt.Errorf("building template registry: %w", err)
	}

	// Fail fast on destination collisions before paying for template rendering
	// or remote secret-provider calls. DestPath is knowable from the file layout alone.
	expanded, err := r.expandAndValidate(fileSet)
	if err != nil {
		return nil, err
	}

	// First render pass: render quadlets with placeholder data to collect
	// secret refs (op://, pass://) for batch resolution and to derive the
	// host's systemd unit names into tmplData.Host.SystemdUnits.
	if _, err := r.prepareTemplateData(ctx, registry, tmplData, expanded, caches); err != nil {
		return nil, err
	}

	// Batch-resolve direct (non-template) secret refs in one call per provider.
	resolvedDirect, err := r.batchResolveDirectSecrets(ctx, secretSources(expanded.Files))
	if err != nil {
		return nil, err
	}

	files, err := r.buildFiles(registry, tmplData, expanded.Files, resolvedDirect)
	if err != nil {
		return nil, err
	}
	hooks, err := r.buildHooks(registry, tmplData, expanded.Hooks, files)
	if err != nil {
		return nil, err
	}

	return &ResolvedHost{
		Hostname:    hostname,
		Host:        host,
		Files:       files,
		Hooks:       hooks,
		DataDir:     r.dataDir,
		HostDataDir: r.hostDataDir,
		SopsFiles:   r.sopsFiles.sorted(),
	}, nil
}

// resolveSopsReader returns the SOPS reader of this host resolve: its keyless
// reader (NewKeyless), else the configured one (nil without one), recording
// the files its refs reach into the resolve's sopsFiles.
func (r *Resolver) resolveSopsReader() SopsReader { //nolint:ireturn // the keyless or the configured reader, two concrete types
	reader := r.sopsReader
	if r.keylessSops {
		reader = r.keylessReader
	}
	if reader == nil {
		return nil
	}
	return sopsFileRecorder{reader: reader, files: r.sopsFiles}
}

// guardedSopsProvider is SopsProvider over this host resolve's SOPS reader
// with the unit-file guard.
func (r *Resolver) guardedSopsProvider() ProviderTemplate {
	p := SopsProvider(r.resolveSopsReader(), r.noSopsHint)
	p.Guard = r.sopsGuard
	return p
}

func (r *Resolver) provider(p ProviderTemplate) ProviderTemplate {
	p.Strict = r.strict
	return p
}

func serviceTemplatePrefixes(services []string) []string {
	prefixes := make([]string, 0, len(services))
	for _, service := range services {
		prefixes = append(prefixes, path.Join(servicesRoot, service)+"/")
	}
	return prefixes
}

func prefixFilter(prefixes []string) func(string) bool {
	if len(prefixes) == 0 {
		return nil
	}
	return func(path string) bool {
		return slices.ContainsFunc(prefixes, func(prefix string) bool {
			return strings.HasPrefix(path, prefix)
		})
	}
}

// preparedData is the output of the first render pass.
//
// Some template data only becomes knowable AFTER quadlet templates render:
//   - Secret refs (op://, pass://) — providers batch better when picolet knows
//     all refs upfront, so we collect them first and resolve in bulk.
//   - Systemd unit names — a quadlet's unit name is derived from its rendered
//     content via Podman's parser, so .Host.SystemdUnits can only be populated
//     AFTER every quadlet has been rendered at least once.
//
// The first pass renders every .tmpl with placeholder data (nil/empty for the
// not-yet-discovered fields). Quadlet renders are parsed to harvest unit names;
// other renders are kept only for their secret-collection side effect. The
// second (final) pass renders for real with fully populated TemplateData.
// Render errors in the first pass are non-fatal; the final pass surfaces real
// template errors with proper diagnostics.
//
// Cost: quadlet templates render once in the first pass for unit discovery,
// plus a second first-pass render for secret collection when secret providers
// are configured, and once more in the final pass. All renders are in-memory;
// cost is negligible vs. git fetches and secret-provider round-trips.
type preparedData struct {
	SystemdUnits []string // sorted, unique; e.g. "node-exporter.service"
}

// prepareTemplateData runs the first render pass: it derives the host's systemd
// unit names, collects secret refs for any configured providers, and resolves
// each provider's cache. It populates tmplData.Host.SystemdUnits as a side
// effect and also returns the result so the pass is testable in isolation.
func (r *Resolver) prepareTemplateData(ctx context.Context, registry *template.Template, tmplData *TemplateData, expanded *expansion, caches ProviderCaches) (*preparedData, error) {
	units := r.collectSystemdUnits(registry, tmplData, expanded.Files)
	if len(caches) > 0 {
		r.collectTemplateRefs(registry, tmplData, expanded)
		if err := caches.ResolveAll(ctx); err != nil {
			return nil, err
		}
	}
	tmplData.Host.SystemdUnits = units
	return &preparedData{SystemdUnits: units}, nil
}

// collectSystemdUnits derives the sorted, unique list of systemd unit names
// picolet manages on the host. Quadlet units (GeneratedUnit categories) are
// rendered and parsed via Podman's GetUnitServiceName, which honors
// ServiceName= overrides; FileUnit categories contribute their filename with
// any .tmpl suffix stripped. Render and parse errors are swallowed here — the
// final pass and the validator surface them with proper diagnostics.
func (r *Resolver) collectSystemdUnits(registry *template.Template, tmplData *TemplateData, refs []fileRef) []string {
	var units []string
	for _, ref := range refs {
		spec, _ := config.SpecFor(ref.Category)
		switch spec.Unit {
		case config.FileUnit:
			units = append(units, destFilename(ref.SrcPath))
		case config.GeneratedUnit:
			f, err := r.resolveFile(registry, tmplData, ref.SrcPath, spec, r.unitDestPath(spec, ref.SrcPath))
			if err == nil && f.ServiceName != "" {
				units = append(units, f.ServiceName)
			}
		case config.NoUnit:
		}
	}
	return config.SortedUnique(units)
}

// ResolveAll resolves all hosts and returns the results.
func (r *Resolver) ResolveAll(ctx context.Context) (map[string]*ResolvedHost, error) {
	results := make(map[string]*ResolvedHost, len(r.cfg.Hosts))
	for _, hostname := range r.cfg.SortedHostnames() {
		resolved, err := r.ResolveHost(ctx, hostname)
		if err != nil {
			return nil, fmt.Errorf("resolving host %s: %w", hostname, err)
		}
		results[hostname] = resolved
	}
	return results, nil
}

// expandAndValidate expands the host's assignment entries into categorized
// files and fails fast if any two sources resolve to the same destination.
func (r *Resolver) expandAndValidate(fileSet *config.ResolvedFileSet) (*expansion, error) {
	expanded, err := r.expandFileSet(fileSet)
	if err != nil {
		return nil, err
	}
	skeletons, err := r.buildFileSkeletons(expanded.Files)
	if err != nil {
		return nil, err
	}
	if err := detectCollisions(skeletons); err != nil {
		return nil, err
	}
	return expanded, nil
}

// expandFileSet expands service bundles and `paths:` entries and adds the
// `secrets:` entries, which are not expanded: they may name host-only
// secrets or provider refs rather than repo files. Files are returned in
// resolution order, each once.
func (r *Resolver) expandFileSet(fileSet *config.ResolvedFileSet) (*expansion, error) {
	expanded, bundleErr := expandServiceBundles(r.fsys, fileSet.Services)
	fromPaths, pathsErr := expandPathEntries(r.fsys, fileSet.Paths)
	if err := errors.Join(bundleErr, pathsErr, rejectPlainSOPSSecrets(fileSet.Secrets)); err != nil {
		return nil, err
	}
	expanded.append(fromPaths)
	for _, src := range fileSet.Secrets {
		expanded.Files = append(expanded.Files, fileRef{SrcPath: src, Category: config.CategorySecret})
	}
	expanded.Files = uniqueFileRefs(expanded.Files)
	return expanded, nil
}

// secretSources returns the source of every secret-category ref.
func secretSources(refs []fileRef) []string {
	var srcs []string
	for _, ref := range refs {
		if ref.Category == config.CategorySecret {
			srcs = append(srcs, ref.SrcPath)
		}
	}
	return srcs
}

// buildFileSkeletons returns SrcPath/Category/DestPath tuples for every file
// the host will deploy. It does not render templates, read files, or call the
// 1Password SDK, so it's safe (and cheap) to run before expensive operations.
func (r *Resolver) buildFileSkeletons(refs []fileRef) ([]ResolvedFile, error) {
	skeletons := make([]ResolvedFile, 0, len(refs))
	for _, ref := range refs {
		dest, err := r.destPath(ref)
		if err != nil {
			return nil, fmt.Errorf("resolving %s %s: %w", ref.Category, ref.SrcPath, err)
		}
		skeletons = append(skeletons, ResolvedFile{
			SrcPath: ref.SrcPath, Category: ref.Category, DestPath: dest, RelPath: ref.RelPath,
		})
	}
	return skeletons, nil
}

// unitDestPath returns the deployed path of a Quadlet or raw systemd source.
func (r *Resolver) unitDestPath(spec config.Spec, srcPath string) string {
	dir := r.quadletDir
	if spec.Dest == config.DestSystemd {
		dir = r.systemdDir
	}
	return filepath.Join(dir, destFilename(srcPath))
}

// destPath returns where ref deploys.
func (r *Resolver) destPath(ref fileRef) (string, error) {
	spec, _ := config.SpecFor(ref.Category)
	switch spec.Dest {
	case config.DestData:
		return r.dataDestPath(ref.DataPath), nil
	case config.DestSecret:
		return r.secretDestPath(ref.SrcPath)
	case config.DestQuadlet, config.DestSystemd:
		return r.unitDestPath(spec, ref.SrcPath), nil
	}
	return "", fmt.Errorf("category %s: unknown destination %d", spec.Category, spec.Dest)
}

func (r *Resolver) dataDestPath(logicalPath string) string {
	return filepath.Join(r.dataDir, filepath.FromSlash(config.TrimTemplateSuffix(logicalPath)))
}

// secretDestPath returns the DestPath for either a provider-backed ref
// (op://, pass:// or sops://) or a file-based secret. Parsing is pure — no
// I/O.
func (r *Resolver) secretDestPath(srcPath string) (string, error) {
	if sops.IsRef(srcPath) {
		parsed, err := sops.ParseRef(srcPath)
		if err != nil {
			return "", err
		}
		name, err := parsed.SecretName()
		if err != nil {
			return "", err
		}
		return "secret:" + name, nil
	}
	if op.IsRef(srcPath) {
		parsed, err := op.ParseOpRef(srcPath)
		if err != nil {
			return "", err
		}
		return "secret:" + parsed.PodmanSecretName(), nil
	}
	if pp.IsRef(srcPath) {
		parsed, err := pp.ParseRef(srcPath)
		if err != nil {
			return "", err
		}
		return "secret:" + parsed.PodmanSecretName(), nil
	}
	filename := destFilename(srcPath)
	return "secret:" + strings.TrimSuffix(filename, filepath.Ext(filename)), nil
}

// buildFiles renders or reads every ref, in resolution order. A provider ref
// whose provider is not configured is skipped (batchResolveDirectSecrets).
func (r *Resolver) buildFiles(
	registry *template.Template,
	tmplData *TemplateData,
	refs []fileRef,
	resolvedDirect map[string]string,
) ([]ResolvedFile, error) {
	files := make([]ResolvedFile, 0, len(refs))
	for _, ref := range refs {
		spec, _ := config.SpecFor(ref.Category)
		var (
			f   *ResolvedFile
			err error
		)
		callsBefore := r.sopsCalls
		switch spec.Dest {
		case config.DestQuadlet, config.DestSystemd:
			f, err = r.resolveFile(registry, tmplData, ref.SrcPath, spec, r.unitDestPath(spec, ref.SrcPath))
		case config.DestData:
			f, err = r.resolveNestedRef(registry, tmplData, ref)
		case config.DestSecret:
			if isProviderRef(ref.SrcPath) {
				if resolvedDirect == nil {
					continue
				}
				f, err = r.buildDirectSecretFile(ref.SrcPath, resolvedDirect[ref.SrcPath])
			} else {
				f, err = r.resolveSecret(registry, tmplData, ref.SrcPath)
			}
		}
		if err != nil {
			return nil, err
		}
		f.SopsValues = r.sopsCalls > callsBefore || sops.IsRef(ref.SrcPath)
		files = append(files, *f)
	}
	return files, nil
}

// isProviderRef reports whether a secrets entry is a provider ref (op://,
// pass://, sops://) rather than a repo or host-only secret file.
func isProviderRef(src string) bool {
	return op.IsRef(src) || pp.IsRef(src) || sops.IsRef(src)
}

func detectCollisions(files []ResolvedFile) error {
	collisions := make(map[string][]string)
	for _, file := range files {
		collisions[file.DestPath] = append(collisions[file.DestPath], file.SrcPath)
	}

	destPaths := make([]string, 0, len(collisions))
	for destPath := range collisions {
		destPaths = append(destPaths, destPath)
	}
	slices.Sort(destPaths)

	var errs []error
	for _, destPath := range destPaths {
		uniquePaths := config.SortedUnique(collisions[destPath])
		if len(uniquePaths) < 2 {
			continue
		}
		errs = append(errs, fmt.Errorf("destination collision for %s: %s", destPath, strings.Join(uniquePaths, ", ")))
	}
	return errors.Join(errs...)
}

func (r *Resolver) resolveFile(registry *template.Template, tmplData *TemplateData, srcPath string, spec config.Spec, destPath string) (*ResolvedFile, error) {
	var (
		content string
		err     error
	)
	r.withRender(guardedRender(srcPath, spec.Dest), func() {
		content, err = r.renderOrRead(registry, tmplData, srcPath)
	})
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", srcPath, err)
	}

	if spec.Dest == config.DestSystemd {
		content = config.PicoletMarker + "\n" + content
	}

	filename := destFilename(srcPath)
	var parsedUnit *parser.UnitFile
	var serviceName string
	switch spec.Unit {
	case config.GeneratedUnit:
		unit := parser.NewUnitFile()
		unit.Filename = filename
		if err := unit.Parse(content); err == nil {
			parsedUnit = unit
			serviceName = unitServiceName(unit)
		}
		// Parse errors are silent here — validator catches them with proper error messages
	case config.FileUnit:
		// Raw systemd units are not parsed; the unit name is the filename. Populate
		// it so the unit is tracked in state.ServiceNames — driving health checks,
		// the status store, and the dashboard, just like quadlet-derived units.
		serviceName = filename
	case config.NoUnit:
	}

	return &ResolvedFile{
		SrcPath:     srcPath,
		DestPath:    destPath,
		Content:     content,
		Category:    spec.Category,
		ParsedUnit:  parsedUnit,
		ServiceName: serviceName,
	}, nil
}

// unitServiceName returns "foo.service" from a parsed quadlet unit, using Podman's
// GetUnitServiceName which handles all quadlet types and ServiceName= overrides.
func unitServiceName(unit *parser.UnitFile) string {
	name, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		return ""
	}
	return name + ".service"
}

// findQuadletFile returns the ResolvedFile whose destination filename matches
// quadletName, or nil if none. Quadlet filename is the source basename minus
// any .tmpl suffix (e.g. "app.container").
func findQuadletFile(quadletName string, files []ResolvedFile) *ResolvedFile {
	for i := range files {
		if destFilename(files[i].SrcPath) == quadletName {
			return &files[i]
		}
	}
	return nil
}

// validateSignalHookContainer cross-checks a signal-action hook's Container
// against the Quadlet [Container] ContainerName= when the unit is
// Quadlet-resolvable and the field is explicitly set. Must be called BEFORE
// resolveHookQuadletUnit rewrites hook.Unit, since the lookup compares against
// the original Quadlet filename (e.g. "app.container").
func (r *Resolver) validateSignalHookContainer(hook config.Hook, files []ResolvedFile) error {
	if hook.Action != config.HookActionSignal || !isQuadletUnit(hook.Unit) {
		return nil
	}
	file := findQuadletFile(hook.Unit, files)
	if file == nil || file.ParsedUnit == nil {
		return nil
	}
	declared, ok := file.ParsedUnit.LookupLast("Container", "ContainerName")
	declared = strings.TrimSpace(declared)
	if !ok || declared == "" {
		r.logger.Debug("Quadlet has no explicit ContainerName, container not validated",
			"hook", hook.Name, "unit", hook.Unit, "container", hook.Container)
		return nil
	}
	if declared != hook.Container {
		return fmt.Errorf("hook %s: container %q does not match Quadlet ContainerName %q for unit %s",
			hook.Name, hook.Container, declared, hook.Unit)
	}
	return nil
}

// resolveHookQuadletUnit finds the ResolvedFile matching the given Quadlet filename
// and returns its ServiceName (computed by the Podman library via GetUnitServiceName).
// Returns an error if no matching file exists in the resolved set.
func resolveHookQuadletUnit(quadletName string, files []ResolvedFile) (string, error) {
	if file := findQuadletFile(quadletName, files); file != nil && file.ServiceName != "" {
		return file.ServiceName, nil
	}
	return "", fmt.Errorf("unit %q: no matching quadlet file found in the host's files", quadletName)
}

// isQuadletUnit reports whether the given unit name has a Quadlet file extension,
// indicating it needs resolution to its generated systemd service name.
func isQuadletUnit(unit string) bool {
	category, ok := config.CategoryForExtension(filepath.Ext(unit))
	if !ok {
		return false
	}
	spec, _ := config.SpecFor(category)
	return spec.Dest == config.DestQuadlet
}

// destFilename returns the base filename for a source path, stripping any .tmpl suffix.
func destFilename(srcPath string) string {
	return config.TrimTemplateSuffix(path.Base(srcPath))
}

func (r *Resolver) resolveNestedRef(registry *template.Template, tmplData *TemplateData, ref fileRef) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, ref.SrcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving %s %s: %w", ref.Category, ref.SrcPath, err)
	}

	return &ResolvedFile{
		SrcPath:  ref.SrcPath,
		DestPath: r.dataDestPath(ref.DataPath),
		Content:  content,
		Category: ref.Category,
		RelPath:  ref.RelPath,
	}, nil
}

func (r *Resolver) buildHooks(registry *template.Template, tmplData *TemplateData, refs []hookRef, files []ResolvedFile) ([]config.Hook, error) {
	type hookOrigin struct {
		service string
		path    string
	}
	var hooks []config.Hook
	seen := make(map[string]hookOrigin)
	for _, ref := range refs {
		fileHooks, err := r.resolveHooksFile(registry, tmplData, ref, files)
		if err != nil {
			return nil, err
		}
		for _, hook := range fileHooks {
			if prev, ok := seen[hook.Name]; ok {
				return nil, fmt.Errorf("%s: duplicate hook name %q (already defined by service %q in %s)", ref.SrcPath, hook.Name, prev.service, prev.path)
			}
			seen[hook.Name] = hookOrigin{service: ref.Service, path: ref.SrcPath}
			hooks = append(hooks, hook)
		}
	}
	return hooks, nil
}

func (r *Resolver) resolveHooksFile(registry *template.Template, tmplData *TemplateData, ref hookRef, files []ResolvedFile) ([]config.Hook, error) {
	var (
		content string
		err     error
	)
	r.withRender(hookRender(ref.SrcPath), func() {
		content, err = r.renderOrRead(registry, tmplData, ref.SrcPath)
	})
	if err != nil {
		return nil, fmt.Errorf("resolving hooks %s: %w", ref.SrcPath, err)
	}
	var file config.HooksFile
	if err := yaml.Load([]byte(content), &file, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ref.SrcPath, err)
	}
	for i := range file.Hooks {
		if err := r.validateSignalHookContainer(file.Hooks[i], files); err != nil {
			return nil, fmt.Errorf("%s: hooks[%d]: %w", ref.SrcPath, i, err)
		}
		if isQuadletUnit(file.Hooks[i].Unit) {
			resolved, err := resolveHookQuadletUnit(file.Hooks[i].Unit, files)
			if err != nil {
				return nil, fmt.Errorf("%s: hooks[%d]: %w", ref.SrcPath, i, err)
			}
			file.Hooks[i].Unit = resolved
		}
		if err := file.Hooks[i].Normalize(); err != nil {
			return nil, fmt.Errorf("%s: hooks[%d]: %w", ref.SrcPath, i, err)
		}
	}
	return file.Hooks, nil
}

func (r *Resolver) resolveSecret(registry *template.Template, tmplData *TemplateData, srcPath string) (*ResolvedFile, error) {
	filename := destFilename(srcPath)
	content, err := r.secretContent(registry, tmplData, srcPath, filename)
	if err != nil {
		return nil, fmt.Errorf("resolving secret %s: %w", srcPath, err)
	}
	destPath, err := r.secretDestPath(srcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving secret %s: %w", srcPath, err)
	}

	return &ResolvedFile{
		SrcPath:  srcPath,
		DestPath: destPath,
		Content:  content,
		Category: config.CategorySecret,
	}, nil
}

// batchResolveDirectSecrets resolves all provider-backed (op://, pass://,
// sops://) refs in the secrets list, one batched call per provider. Returns
// a single map keyed by ref. A resolution failure is fatal to prevent
// reconciler.Diff from marking unresolved secrets for deletion (which would
// remove them from Podman). When op:// or pass:// is not configured, its
// refs are skipped with a warning so the rest of the reconcile can proceed;
// sops:// refs never are (resolveSopsDirectRefs).
//
//nolint:nilnil // nil map signals "no provider-backed secrets to resolve"
func (r *Resolver) batchResolveDirectSecrets(ctx context.Context, allSecrets []string) (map[string]string, error) {
	refs := splitDirectRefs(allSecrets)
	if len(refs.op) == 0 && len(refs.pp) == 0 && len(refs.sops) == 0 {
		return nil, nil
	}

	results := make(map[string]string)
	if err := r.resolveProviderRefs(ctx, ProviderOnePassword, r.opSecretReader, refs.op, results); err != nil {
		return nil, err
	}
	if err := r.resolveProviderRefs(ctx, ProviderProtonPass, r.ppSecretReader, refs.pp, results); err != nil {
		return nil, err
	}
	if err := r.resolveSopsDirectRefs(ctx, refs.sops, results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results, nil
}

// directRefs are a secrets list's provider refs, split by scheme.
type directRefs struct {
	op, pp, sops []string
}

func splitDirectRefs(allSecrets []string) directRefs {
	var refs directRefs
	for _, path := range allSecrets {
		switch {
		case op.IsRef(path):
			refs.op = append(refs.op, path)
		case pp.IsRef(path):
			refs.pp = append(refs.pp, path)
		case sops.IsRef(path):
			refs.sops = append(refs.sops, path)
		}
	}
	return refs
}

// resolveSopsDirectRefs resolves direct sops:// refs into into, through the
// host resolve's SOPS reader (directSopsReader), in the same batch as the
// other providers. Unlike op:// and pass://, they are never skipped: strict
// mode fails them even with the keyless reader, and without a reader they
// fail with the no-identity error, so a placeholder is never deployed as a
// value.
func (r *Resolver) resolveSopsDirectRefs(ctx context.Context, refs []string, into map[string]string) error {
	if len(refs) == 0 {
		return nil
	}
	if r.strict {
		return strictDirectError(ProviderSops, refs)
	}
	reader := r.directSopsReader()
	if reader == nil {
		return sops.NoIdentityError("direct secrets "+strings.Join(refs, ", "), r.noSopsHint)
	}
	r.logger.Debug("batch-resolving secrets", "provider", ProviderSops, "count", len(refs))
	results, err := reader(ctx, refs)
	if err != nil {
		return fmt.Errorf("resolving %s secrets: %w", ProviderSops, err)
	}
	maps.Copy(into, results)
	return nil
}

// directSopsReader returns the reader of this host resolve's direct
// sops:// refs, nil without one: the resolve's keyless reader in direct
// mode (NewKeyless), else the configured reader with an empty value
// rejected. Either way each ref's path is checked like a template ref's
// (checkSopsRefs).
func (r *Resolver) directSopsReader() SecretRefReader {
	if r.keylessSops {
		return checkSopsRefs(r.sopsFiles.recordRefs(r.keylessReader.ReadDirect))
	}
	if r.sopsReader == nil {
		return nil
	}
	return checkSopsRefs(r.sopsFiles.recordRefs(rejectEmptyDirect(r.sopsReader.ReadAll)))
}

// rejectEmptyDirect wraps a decrypting reader so that a ref resolving to ""
// (an "" or null leaf, which SOPS leaves unencrypted) fails: a direct
// secrets: entry needs a non-empty value.
func rejectEmptyDirect(reader SecretRefReader) SecretRefReader {
	return func(ctx context.Context, refs []string) (map[string]string, error) {
		values, err := reader(ctx, refs)
		var errs []error
		if err != nil {
			errs = append(errs, err)
		}
		for _, ref := range refs {
			if v, ok := values[ref]; ok && v == "" {
				delete(values, ref)
				errs = append(errs, sops.EmptyDirectValueError(ref))
			}
		}
		return values, errors.Join(errs...)
	}
}

func (r *Resolver) resolveProviderRefs(ctx context.Context, name ProviderKey, reader SecretRefReader, refs []string, into map[string]string) error {
	if len(refs) == 0 {
		return nil
	}
	if reader == nil {
		if r.strict {
			return strictDirectError(name, refs)
		}
		r.logger.Warn("skipping secrets (provider not configured)", "provider", name, "count", len(refs))
		return nil
	}
	r.logger.Debug("batch-resolving secrets", "provider", name, "count", len(refs))
	results, err := reader(ctx, refs)
	if err != nil {
		return fmt.Errorf("resolving %s secrets: %w", name, err)
	}
	maps.Copy(into, results)
	return nil
}

// collectTemplateRefs executes all .tmpl files in collect mode to discover
// reader-function calls (readOpSecret, readProtonPassSecret, …). Output is
// discarded — only the side effect of populating each provider's RefCache matters.
// Secret templates are included: they may call provider reader functions;
// direct provider refs (op://, pass://, sops://) are not templates and are
// skipped.
func (r *Resolver) collectTemplateRefs(registry *template.Template, tmplData *TemplateData, expanded *expansion) {
	collect := func(path string, g *unitRender) {
		if !config.IsTemplate(path) {
			return
		}
		// The unit-file guard runs here too, so a guarded file's refs are
		// never collected and never reach a decrypting reader.
		r.withRender(g, func() {
			_ = registry.ExecuteTemplate(io.Discard, path, tmplData) // errors are non-fatal in collect phase
		})
	}
	for _, ref := range expanded.Files {
		if isProviderRef(ref.SrcPath) {
			continue
		}
		spec, ok := config.SpecFor(ref.Category)
		if !ok {
			// No row, no known guard: skip collecting (fail closed), so no
			// ref of it reaches a decrypting reader; resolving it fails later.
			continue
		}
		collect(ref.SrcPath, guardedRender(ref.SrcPath, spec.Dest))
	}
	for _, ref := range expanded.Hooks {
		collect(ref.SrcPath, hookRender(ref.SrcPath))
	}
}

// buildDirectSecretFile creates a ResolvedFile for a pre-resolved
// provider-backed secret (op://, pass:// or sops://).
func (r *Resolver) buildDirectSecretFile(ref, content string) (*ResolvedFile, error) {
	destPath, err := r.secretDestPath(ref)
	if err != nil {
		return nil, err
	}
	return &ResolvedFile{
		SrcPath:  ref,
		DestPath: destPath,
		Content:  content,
		Category: config.CategorySecret,
	}, nil
}

// secretContent returns the content for a secret entry.
// Modes, in priority order:
//  1. Template secrets (.tmpl suffix) are rendered with the full template engine.
//  2. Static repo secrets (file exists in repo without .tmpl) are copied as-is.
//  3. Host-only secrets (not in repo) are read from SecretsDir via secretReader.
//  4. If no secretReader is configured, a placeholder value ("<secret>") is returned and a warning is logged.
func (r *Resolver) secretContent(registry *template.Template, tmplData *TemplateData, srcPath, filename string) (string, error) {
	if config.IsTemplate(srcPath) {
		return r.renderOrRead(registry, tmplData, srcPath)
	}
	// Static repo file — copy as-is without template rendering.
	data, readErr := fs.ReadFile(r.fsys, srcPath)
	if readErr == nil {
		r.logger.Debug("reading static secret from repo", "path", srcPath)
		return string(data), nil
	}
	if !errors.Is(readErr, fs.ErrNotExist) {
		return "", fmt.Errorf("reading static secret %s: %w", srcPath, readErr)
	}
	// Host-only secret (API keys, tokens) — read from SecretsDir.
	if r.secretReader != nil {
		return r.secretReader(filename)
	}
	r.logger.Warn("secret reader not configured, using placeholder", "file", srcPath)
	return placeholderSecret, nil
}

func (r *Resolver) renderOrRead(registry *template.Template, tmplData *TemplateData, path string) (string, error) {
	if config.IsTemplate(path) {
		r.logger.Debug("rendering template", "path", path)
		var buf bytes.Buffer
		if err := registry.ExecuteTemplate(&buf, path, tmplData); err != nil {
			return "", fmt.Errorf("executing template %s: %w", path, err)
		}
		return buf.String(), nil
	}

	r.logger.Debug("reading static file", "path", path)
	data, err := fs.ReadFile(r.fsys, path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(data), nil
}
