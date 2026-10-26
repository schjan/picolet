package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/template"

	sprig "github.com/Masterminds/sprig/v3"

	"github.com/schjan/picolet/pkg/config"
	op "github.com/schjan/picolet/pkg/onepassword"
	pp "github.com/schjan/picolet/pkg/protonpass"
	"github.com/schjan/picolet/pkg/sops"
)

// dataPathFunc returns the spec.PathHelper template func, resolving a
// relative path inside a data category's deployed subdirectory (e.g.
// dataDir/manifests/...).
func dataPathFunc(dataDir string, spec config.Spec) func(string) (string, error) {
	return func(relPath string) (string, error) {
		cleaned, err := config.ValidateRelPath(relPath)
		if err != nil {
			return "", fmt.Errorf("%s %q: %w", spec.PathHelper, relPath, err)
		}
		return spec.DataPath(dataDir, cleaned), nil
	}
}

const maxTemplateDepth = 10

// SecretReader reads secret file content.
// Pass nil for placeholder mode (validate/CI).
type SecretReader func(path string) (string, error)

// SecretRefReader resolves secret references in batch (e.g. op:// or pass://).
// Returns successfully resolved secrets and per-reference errors separately.
// Pass nil to disable a provider (its template function returns a placeholder).
type SecretRefReader func(ctx context.Context, refs []string) (map[string]string, error)

// SecretDataReader resolves one secret reference to structured data: a
// map[string]any, a []any or a string (readSopsData).
type SecretDataReader func(ctx context.Context, ref string) (any, error)

// SopsReader resolves sops:// refs: the strings of readSopsSecret, in batch
// (ReadAll), and the subtrees of readSopsData (ReadData). One SopsReader
// serves one resolve and caches per file, so a file both helpers read is
// read (and, on the Agent, decrypted) once.
type SopsReader interface {
	ReadAll(ctx context.Context, refs []string) (map[string]string, error)
	ReadData(ctx context.Context, ref string) (any, error)
}

// ProviderKey identifies a secret provider inside the template registry.
type ProviderKey string

const (
	ProviderOnePassword ProviderKey = "onepassword"
	ProviderProtonPass  ProviderKey = "protonpass"
	ProviderSops        ProviderKey = "sops"
)

// ProviderTemplate describes a secret-provider integration for the template registry.
//
// Key is used only to return the provider's cache without relying on slice order.
// FuncName is the Go template function name (e.g. "readOpSecret").
// IsRef returns true when a string is a syntactically valid reference for the provider.
// PlaceholderEmpty is returned when the provider is not configured (Reader == nil),
// unless MissingReader is set: then its error fails the render instead.
// PlaceholderPending is returned during the collect phase before refs are resolved.
// DataFuncName, when set, names a second template function returning
// structured data through DataReader (readSopsData). It reads each ref
// directly, in the collect pass too (ranging over a pending placeholder
// would fail), so DataReader must cache per file itself.
// Strict (bootstrap, validate's Agent-bundle pass) fails every call.
// Guard, when set, runs first on every call; its error fails the render
// before any reader sees the ref (the SOPS unit-file guard).
// MissingReader and Guard get the called function's name.
//
// The standard providers (OpProvider, PPProvider, SopsProvider) also carry
// their scheme's static data, looked up through providerSchemes: the Podman
// secret a direct secrets: ref deploys as, and strict mode's wording.
type ProviderTemplate struct {
	Key                ProviderKey
	FuncName           string
	Reader             SecretRefReader
	DataFuncName       string
	DataReader         SecretDataReader
	IsRef              func(string) bool
	PlaceholderEmpty   string
	PlaceholderPending string
	MissingReader      func(funcName, ref string) error
	Strict             bool
	Guard              func(funcName, ref string) error

	// secretName is the Podman secret name a direct secrets: ref deploys as.
	secretName func(ref string) (string, error)
	// strictReason and strictFix word a strict-mode failure (strictText):
	// why a call fails there and how the value must reach the Agent instead.
	strictReason, strictFix string
	// directLikeCalls words strict mode's direct-ref error like a template
	// call's (sops://); op:// and pass:// keep the wording they had before
	// sops:// (spec #248).
	directLikeCalls bool
}

// providerSchemes is the provider table: the standard providers without
// readers, one per ref scheme, built once. isProviderRef, splitDirectRefs,
// secretDestPath and the strict-mode errors look a scheme up here.
var providerSchemes = []ProviderTemplate{OpProvider(nil), PPProvider(nil), SopsProvider(nil, "")}

// schemeOfRef returns the provider whose IsRef accepts ref.
func schemeOfRef(ref string) (ProviderTemplate, bool) {
	for _, p := range providerSchemes {
		if p.IsRef(ref) {
			return p, true
		}
	}
	return ProviderTemplate{}, false
}

// schemeOf returns the provider table's entry for key; a key without one
// gets an entry with only the key (strict mode's default wording).
func schemeOf(key ProviderKey) ProviderTemplate {
	for _, p := range providerSchemes {
		if p.Key == key {
			return p
		}
	}
	return ProviderTemplate{Key: key}
}

// legacyStrictFix is op:// and pass://'s strict-mode fix.
const legacyStrictFix = "use a YAML-value reference and let the agent resolve it at runtime"

// OpProvider returns the standard 1Password provider template.
// reader may be nil to disable the provider.
func OpProvider(reader SecretRefReader) ProviderTemplate {
	return ProviderTemplate{
		Key:                ProviderOnePassword,
		FuncName:           "readOpSecret",
		Reader:             reader,
		IsRef:              op.IsRef,
		PlaceholderEmpty:   "<op-secret>",
		PlaceholderPending: "<op-secret-pending>",
		secretName: func(ref string) (string, error) {
			parsed, err := op.ParseOpRef(ref)
			if err != nil {
				return "", err
			}
			return parsed.PodmanSecretName(), nil
		},
		strictFix: legacyStrictFix,
	}
}

// PPProvider returns the standard Proton Pass provider template.
// reader may be nil to disable the provider.
func PPProvider(reader SecretRefReader) ProviderTemplate {
	return ProviderTemplate{
		Key:                ProviderProtonPass,
		FuncName:           "readProtonPassSecret",
		Reader:             reader,
		IsRef:              pp.IsRef,
		PlaceholderEmpty:   "<pp-secret>",
		PlaceholderPending: "<pp-secret-pending>",
		secretName: func(ref string) (string, error) {
			parsed, err := pp.ParseRef(ref)
			if err != nil {
				return "", err
			}
			return parsed.PodmanSecretName(), nil
		},
		strictFix: legacyStrictFix,
	}
}

// SopsProvider returns the SOPS provider template: readSopsSecret (strings)
// and readSopsData (subtrees), both over reader. reader is the keyless
// reader (validate, resolve, bootstrap) or the decrypting one (Agent); with
// a nil reader every ref fails with the no-identity error naming
// noIdentityHint, so a placeholder can never be deployed as a value. Each
// ref's path is checked like readFile's (Fleet-relative, no "..") before
// reader sees it.
func SopsProvider(reader SopsReader, noIdentityHint string) ProviderTemplate {
	p := ProviderTemplate{
		Key:                ProviderSops,
		FuncName:           "readSopsSecret",
		DataFuncName:       "readSopsData",
		IsRef:              sops.IsRef,
		PlaceholderPending: "<sops-secret-pending>",
		MissingReader: func(funcName, ref string) error {
			return sops.NoIdentityError(fmt.Sprintf("%s %q", funcName, refForError(ref)), noIdentityHint)
		},
		secretName: func(ref string) (string, error) {
			parsed, err := sops.ParseRef(ref)
			if err != nil {
				return "", err
			}
			return parsed.SecretName()
		},
		strictReason: "SOPS values can't be used in strict mode",
		strictFix: "deliver the value as a --secrets-dir file and point the Agent config at its path; " +
			"credentials needed to fetch the Fleet can't be SOPS values",
		directLikeCalls: true,
	}
	if reader != nil {
		p.Reader = checkSopsRefs(reader.ReadAll)
		p.DataReader = func(ctx context.Context, ref string) (any, error) {
			if err := checkSopsRef(ref); err != nil {
				return nil, err
			}
			return reader.ReadData(ctx, ref)
		}
	}
	return p
}

// keylessSopsReader returns the keyless SOPS reader over fsys (the Fleet
// root) for host, the reader of NewKeyless's host resolves: it checks each
// ref without any key, against host's age_recipients: too, and renders
// placeholders (sops.KeylessReader).
func keylessSopsReader(fsys fs.FS, host *config.HostConfig) *sops.KeylessReader {
	return sops.NewKeylessReader(fsys, sops.Host{Name: host.Hostname, Recipients: host.AgeRecipients})
}

// checkSopsRef fails, naming the ref, when raw is not a sops:// ref or its
// file path is not a clean Fleet-relative path.
func checkSopsRef(raw string) error {
	ref, err := sops.ParseRef(raw)
	if err != nil {
		return err
	}
	if _, err := config.ValidateRelPath(ref.Path); err != nil {
		return fmt.Errorf("%s: %w", raw, err)
	}
	return nil
}

// checkSopsRefs wraps reader so that a ref checkSopsRef fails never reaches
// reader.
func checkSopsRefs(reader SecretRefReader) SecretRefReader {
	return func(ctx context.Context, refs []string) (map[string]string, error) {
		var valid []string
		var errs []error
		for _, raw := range refs {
			if err := checkSopsRef(raw); err != nil {
				errs = append(errs, err)
				continue
			}
			valid = append(valid, raw)
		}
		values := map[string]string{}
		if len(valid) > 0 {
			var err error
			values, err = reader(ctx, valid)
			errs = append(errs, err)
		}
		return values, errors.Join(errs...)
	}
}

// strictText is the one place strict mode (bootstrap, validate's
// Agent-bundle pass) words p's failure: why a call fails there (reason) and
// how the value must reach the Agent instead (fix).
func (p ProviderTemplate) strictText() (reason, fix string) {
	reason, fix = p.strictReason, p.strictFix
	if reason == "" {
		reason = "provider not configured in strict mode"
	}
	if fix == "" {
		fix = "this provider cannot be resolved in strict mode"
	}
	return reason, fix
}

// refForError names ref in an error without the part a template most likely
// builds from a secret value: a sops:// ref keeps its file, an op:// or
// pass:// ref its vault (share), and "…" marks the dropped pointer, or item
// and field. Errors that may quote a ref built at render time use it, since
// only SOPS values are scrubbed from errors.
func refForError(ref string) string {
	if sops.IsRef(ref) {
		if file, _, found := strings.Cut(ref, "#"); found {
			return file + "#…"
		}
		return ref // the whole file
	}
	scheme, rest, _ := strings.Cut(ref, "://")
	if vault, _, found := strings.Cut(rest, "/"); found && vault != "" {
		return scheme + "://" + vault + "/…"
	}
	return scheme + "://…"
}

// redactRefs returns err with every op:// or pass:// ref of refs it quotes
// replaced by refForError's form: a provider client's error quotes the refs
// it failed to read, and a template may build one from a secret value. A
// sops:// ref is kept, so a SOPS reader's error still names the pointer it
// failed at. The result answers errors.Is like err but does not unwrap.
func redactRefs(err error, refs []string) error {
	return redact(err, refs, func(ref string) bool { return !sops.IsRef(ref) })
}

// dynamicRefError is the error of funcName's reader failing on ref, a ref
// built at render time (one the collect pass did not see): the template may
// have built it from an op:// or pass:// value, which no SOPS scrubbing
// removes, and a reader quotes more of a ref than the ref itself (a SOPS
// reader the pointer and its failing key, a provider client the item), so
// the reader's text is withheld. The error names funcName, refForError's form
// of ref and the SOPS error class err falls in, if any; it answers
// errors.Is like err but does not unwrap.
func dynamicRefError(funcName, ref string, err error) error {
	if err == nil {
		return nil
	}
	msg := fmt.Sprintf("%s %q: ", funcName, refForError(ref))
	for _, class := range sopsErrorClasses {
		if errors.Is(err, class) {
			msg += class.Error() + "; "
			break
		}
	}
	msg += "the reader's error is withheld, since the ref was built at render time and may hold a secret value"
	return redactedError{msg: msg, err: err}
}

// sopsErrorClasses are the SOPS readers' fixed error classes, whose own text
// holds no ref, pointer or value.
var sopsErrorClasses = []error{
	sops.ErrNoIdentity, sops.ErrNotRecipient, sops.ErrEditedOutsideSOPS, sops.ErrBadPointer, sops.ErrNotLeaf,
	sops.ErrNotSOPSPath, sops.ErrNotSOPSFile, sops.ErrFileTooLarge, sops.ErrEmptyValue,
}

// redact returns err with every ref of refs it quotes for which redacted
// reports true replaced by refForError's form (a redactedError), or err
// itself when it quotes none. A ref is found as written and as %q escapes
// it (the provider clients quote refs that way), the longest first, so a
// ref that is a prefix of another never leaves the longer one's tail behind.
func redact(err error, refs []string, redacted func(string) bool) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	refs = slices.Clone(refs)
	slices.SortStableFunc(refs, func(a, b string) int { return len(b) - len(a) })
	for _, ref := range refs {
		if redacted(ref) {
			quoted := strconv.Quote(ref)
			msg = strings.ReplaceAll(msg, quoted[1:len(quoted)-1], refForError(ref))
			msg = strings.ReplaceAll(msg, ref, refForError(ref))
		}
	}
	if msg == err.Error() {
		return err
	}
	return redactedError{msg: msg, err: err}
}

// redactedError is an error whose text redactRefs rewrote. It deliberately
// does not unwrap, so no caller reaches the original text.
type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }

// Is reports whether the original error matches target.
func (e redactedError) Is(target error) bool { return errors.Is(e.err, target) }

// strictModeError is the error a call of p's function funcName fails with
// in strict mode, where no provider value can be used. The wording is the
// table's for p.Key, as strict mode has always keyed it, so a template built
// by hand under a standard key words it like the standard one.
func (p ProviderTemplate) strictModeError(funcName, ref string) error {
	reason, fix := schemeOf(p.Key).strictText()
	return fmt.Errorf("%s %q: %s; %s", funcName, refForError(ref), reason, fix)
}

// strictDirectError is the error p's direct secrets: refs fail with in
// strict mode. op:// and pass:// keep the wording they had before sops://
// (spec #248) with the fix appended; sops:// words it like its template
// calls.
func (p ProviderTemplate) strictDirectError(refs []string) error {
	reason, fix := p.strictText()
	joined := strings.Join(refs, ", ")
	if p.directLikeCalls {
		return fmt.Errorf("direct secrets %s: %s; %s", joined, reason, fix)
	}
	return fmt.Errorf("%s provider not configured; cannot resolve direct secrets in strict mode: %s; %s", p.Key, joined, fix)
}

const placeholderSecret = "<secret>"

// RefCache manages two-phase resolution of secret references for templates.
// Avoids N individual provider round-trips when templates use multiple
// reader-function calls.
//
// Not goroutine-safe; template rendering must be serial (same constraint as renderTemplate).
type RefCache struct {
	reader    SecretRefReader
	logger    *slog.Logger
	collected []string
	resolved  map[string]string // non-nil after Resolve(); doubles as phase indicator
	// dataRefs are the refs the provider's data function read before
	// Resolve (registerDataFunc).
	dataRefs map[string]bool
}

// ProviderCaches stores per-provider caches by key so callers do not depend on
// the provider slice order.
type ProviderCaches map[ProviderKey]*RefCache

// ResolveAll resolves all configured provider caches.
func (c ProviderCaches) ResolveAll(ctx context.Context) error {
	for _, key := range sortedCacheKeys(c) {
		if err := c[key].Resolve(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Resolve batch-resolves all collected refs. After this call, the cache returns
// resolved values for any subsequent template execution.
func (c *RefCache) Resolve(ctx context.Context) error {
	if len(c.collected) == 0 {
		c.resolved = make(map[string]string)
		return nil
	}
	unique := config.SortedUnique(c.collected)
	c.log().Debug("batch-resolving template secrets", "count", len(unique))
	results, err := c.reader(ctx, unique)
	if err != nil {
		return fmt.Errorf("resolving template secrets: %w", redactRefs(err, unique))
	}
	c.resolved = results
	return nil
}

// log returns the cache's logger, slog.Default() when none was set.
func (c *RefCache) log() *slog.Logger {
	if c.logger == nil {
		return slog.Default()
	}
	return c.logger
}

// BuildRegistry collects all .tmpl files from the filesystem and builds
// a shared template registry with Sprig + picolet-specific functions.
//
// For each ProviderTemplate with a non-nil Reader, a RefCache is created and
// the corresponding template function is registered. The returned caches map
// contains entries only for providers whose Reader is non-nil.
//
// Two-phase resolution: callers must run a collect pass, call cache.Resolve,
// then run the real render pass.
func BuildRegistry(ctx context.Context, fsys fs.FS, secretReader SecretReader, providers []ProviderTemplate, dataDir string) (*template.Template, ProviderCaches, error) {
	return buildRegistry(ctx, fsys, secretReader, providers, dataDir, nil, nil, slog.Default())
}

// buildRegistry is BuildRegistry with an optional source-path predicate and
// the current Host's siblings. A nil include function includes every
// template. The registry is rebuilt per resolve, so the `siblings` helper is
// a per-Host closure over hostSiblings. The provider caches log through
// logger.
//
//nolint:cyclop,funlen // funcmap registration is inherently branchy
func buildRegistry(ctx context.Context, fsys fs.FS, secretReader SecretReader, providers []ProviderTemplate, dataDir string, include func(string) bool, hostSiblings []HostTemplateData, logger *slog.Logger) (*template.Template, ProviderCaches, error) {
	sources, err := loadTemplateSources(fsys, include)
	if err != nil {
		return nil, nil, err
	}
	if err := validateProviderKeys(providers); err != nil {
		return nil, nil, err
	}

	caches := make(ProviderCaches)
	for _, p := range providers {
		if p.Reader != nil {
			caches[p.Key] = &RefCache{reader: p.Reader, logger: logger}
		}
	}

	var root *template.Template
	funcMap := sprig.HermeticTxtFuncMap()
	maps.Copy(funcMap, template.FuncMap{
		// Keep historical picolet semantics: do not indent empty lines.
		"indent":  indentFunc,
		"nindent": nindentFunc,
		"readFile": func(path string) (string, error) {
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return "", fmt.Errorf("readFile %q: %w", path, err)
			}
			return string(data), nil
		},
		"glob": func(patterns ...string) ([]string, error) {
			return globFunc(fsys, patterns...)
		},
		"concatFiles": func(patterns ...string) (string, error) {
			return concatFilesFunc(fsys, patterns...)
		},
		"readSecretFile": func(path string) (string, error) {
			if secretReader == nil {
				return placeholderSecret, nil
			}
			return secretReader(path)
		},
		// siblings lists the other Hosts on this Host's Machine, sorted by hostname.
		"siblings": func() []HostTemplateData { return hostSiblings },
		// renderTemplate uses a closure depth counter to prevent infinite recursion.
		// Not goroutine-safe; template rendering must be serial.
		"renderTemplate": func() any {
			var depth int
			return func(name string, data any) (string, error) {
				depth++
				if depth > maxTemplateDepth {
					return "", fmt.Errorf("renderTemplate %q: recursion depth exceeded (%d)", name, maxTemplateDepth)
				}
				defer func() { depth-- }()
				var buf bytes.Buffer
				if err := root.ExecuteTemplate(&buf, name, data); err != nil {
					return "", fmt.Errorf("renderTemplate %q: %w", name, err)
				}
				return buf.String(), nil
			}
		}(),
	})
	for _, spec := range config.Specs() {
		if spec.PathHelper != "" {
			funcMap[spec.PathHelper] = dataPathFunc(dataDir, spec)
		}
	}

	for i := range providers {
		registerProviderFunc(ctx, funcMap, providers[i], caches[providers[i].Key])
	}

	root = template.New("").Option("missingkey=error").Funcs(funcMap)
	for name, src := range sources {
		if _, err := root.New(name).Parse(src); err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", name, err)
		}
	}
	return root, caches, nil
}

func validateProviderKeys(providers []ProviderTemplate) error {
	seen := make(map[ProviderKey]string, len(providers))
	for _, p := range providers {
		if p.Key == "" {
			return fmt.Errorf("provider %q has empty key", p.FuncName)
		}
		if prev, ok := seen[p.Key]; ok {
			return fmt.Errorf("provider key %q used by both %q and %q", p.Key, prev, p.FuncName)
		}
		seen[p.Key] = p.FuncName
	}
	return nil
}

func sortedCacheKeys(c ProviderCaches) []ProviderKey {
	keys := slices.Collect(maps.Keys(c))
	slices.Sort(keys)
	return keys
}

func loadTemplateSources(fsys fs.FS, include func(string) bool) (map[string]string, error) {
	sources := make(map[string]string)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if d.IsDir() || !config.IsTemplate(path) || (include != nil && !include(path)) {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		sources[path] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking templates: %w", err)
	}
	return sources, nil
}

// registerProviderFunc adds a provider's template function to funcMap.
// In strict mode every call fails, even when the provider has a Reader:
// bootstrap and validate's Agent-bundle pass render what the Agent gets
// before it has any provider, and they pass the keyless SOPS reader. When the provider's Reader is nil
// (cache == nil), the function returns the configured "empty" placeholder, or
// MissingReader's error when set. Otherwise it participates in the two-phase
// collect+resolve cycle.
func registerProviderFunc(ctx context.Context, funcMap template.FuncMap, p ProviderTemplate, cache *RefCache) {
	if p.DataFuncName != "" {
		registerDataFunc(ctx, funcMap, p, cache)
	}
	funcMap[p.FuncName] = func(ref string) (string, error) {
		if ready, err := p.prepareCall(p.FuncName, ref, cache != nil); !ready {
			return p.PlaceholderEmpty, err
		}
		if cache.resolved == nil {
			cache.collected = append(cache.collected, ref)
			return p.PlaceholderPending, nil
		}
		if val, ok := cache.resolved[ref]; ok {
			return val, nil
		}
		// Cache miss: dynamic ref not seen in collect phase. The ref is not
		// logged: a template may build it from a secret value.
		cache.log().Debug("cache miss, resolving individually", "func", p.FuncName)
		results, err := p.Reader(ctx, []string{ref})
		if err != nil {
			return "", dynamicRefError(p.FuncName, ref, err)
		}
		val, ok := results[ref]
		if !ok {
			return "", fmt.Errorf("%s: a ref built at render time failed to resolve", p.FuncName)
		}
		return val, nil
	}
}

// registerDataFunc adds the provider's data function (p.DataFuncName) to
// funcMap: the same prechecks, missing-reader error and ref check as the
// string function, then p.DataReader's structure for ref. It skips the
// two-phase cache's values: every call, in the collect pass too, reads
// through DataReader, which caches per file. The cache (nil without one)
// only records the refs the collect pass read; a ref the render pass reads
// that the collect pass did not was built at render time, maybe from a
// secret value, so its reader error is withheld (dynamicRefError).
func registerDataFunc(ctx context.Context, funcMap template.FuncMap, p ProviderTemplate, cache *RefCache) {
	funcMap[p.DataFuncName] = func(ref string) (any, error) {
		if ready, err := p.prepareCall(p.DataFuncName, ref, p.DataReader != nil); !ready {
			return p.PlaceholderEmpty, err
		}
		collecting := cache != nil && cache.resolved == nil
		if collecting {
			if cache.dataRefs == nil {
				cache.dataRefs = map[string]bool{}
			}
			cache.dataRefs[ref] = true
		}
		data, err := p.DataReader(ctx, ref)
		if err != nil && !collecting && (cache == nil || !cache.dataRefs[ref]) {
			return nil, dynamicRefError(p.DataFuncName, ref, err)
		}
		return data, err
	}
}

// prepareCall runs the checks every provider function makes before reading
// ref: precheck, then, without a reader (hasReader false), MissingReader's
// error, then the ref check. ready is true only when the caller should read
// ref; when it is false with a nil error the caller returns
// PlaceholderEmpty (a provider without a reader and without MissingReader).
func (p ProviderTemplate) prepareCall(funcName, ref string, hasReader bool) (ready bool, err error) {
	if err := p.precheck(funcName, ref); err != nil {
		return false, err
	}
	if !hasReader {
		if p.MissingReader != nil {
			return false, p.MissingReader(funcName, ref)
		}
		return false, nil
	}
	if !p.IsRef(ref) {
		return false, fmt.Errorf("%s: the argument is not a valid reference for this provider", funcName)
	}
	return true, nil
}

// precheck fails a call of funcName before any reader or cache sees ref:
// the Guard first, then strict mode.
func (p ProviderTemplate) precheck(funcName, ref string) error {
	if p.Guard != nil {
		if err := p.Guard(funcName, ref); err != nil {
			return err
		}
	}
	if p.Strict {
		return p.strictModeError(funcName, ref)
	}
	return nil
}

func indentFunc(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func nindentFunc(n int, s string) string {
	return "\n" + indentFunc(n, s)
}

func globFunc(fsys fs.FS, patterns ...string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("glob: at least one pattern is required")
	}

	var paths []string
	for _, pattern := range patterns {
		matches, err := fs.Glob(fsys, pattern)
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("glob %q: no files matched", pattern)
		}
		paths = append(paths, matches...)
	}

	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func concatFilesFunc(fsys fs.FS, patterns ...string) (string, error) {
	paths, err := globFunc(fsys, patterns...)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	var prev string
	for _, path := range paths {
		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return "", fmt.Errorf("concatFiles %q: %w", path, readErr)
		}
		current := string(data)
		if needsNewlineSeparator(prev, current) {
			b.WriteByte('\n')
		}
		b.WriteString(current)
		prev = current
	}

	return b.String(), nil
}

func needsNewlineSeparator(left, right string) bool {
	return left != "" &&
		right != "" &&
		!strings.HasSuffix(left, "\n") &&
		!strings.HasPrefix(right, "\n")
}
