package config

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	HookActionHTTP    = "http"
	HookActionSignal  = "signal"
	HookActionRestart = "restart"

	HookOnFailureKeepRunning = "keep_running"
	HookOnFailureRestart     = "restart"
)

const DefaultMaxRetries = 10

// Hook describes a runtime action to run after one or more Podman secrets
// or manifest files change.
type Hook struct {
	Name       string   `yaml:"name"`
	Secrets    []string `yaml:"secrets"`
	Manifests  []string `yaml:"manifests"`
	Files      []string `yaml:"files"`
	Unit       string   `yaml:"unit"`
	Action     string   `yaml:"action"`
	Method     string   `yaml:"method"`
	URL        string   `yaml:"url"`
	HealthURL  string   `yaml:"health_url"`
	Container  string   `yaml:"container"`
	Signal     string   `yaml:"signal"`
	OnFailure  string   `yaml:"on_failure"`
	MaxRetries int      `yaml:"max_retries"`
}

// FallbackToRestart reports whether a hook execution failure should fall back
// to restarting hook.Unit (rather than leaving the unit running and retrying
// the hook on the next tick).
func (h Hook) FallbackToRestart() bool {
	return h.OnFailure == HookOnFailureRestart
}

// HooksFile is the service-bundle metadata file schema.
type HooksFile struct {
	Hooks []Hook `yaml:"hooks"`
}

type hookField struct {
	name  string
	value string
}

// Normalize applies defaults and validates the hook.
//
//nolint:cyclop // sequential validation steps are clearer as a single flow
func (h *Hook) Normalize() error {
	if h.Name == "" {
		return errors.New("name is required")
	}
	if len(h.Secrets) == 0 && len(h.Manifests) == 0 && len(h.Files) == 0 {
		return fmt.Errorf("%s: at least one of secrets, manifests, or files is required", h.Name)
	}
	if h.Unit == "" {
		return fmt.Errorf("%s: unit is required", h.Name)
	}
	if h.Action == "" {
		return fmt.Errorf("%s: action is required", h.Name)
	}
	h.OnFailure = cmp.Or(h.OnFailure, HookOnFailureKeepRunning)
	if h.MaxRetries == 0 {
		h.MaxRetries = DefaultMaxRetries
	}
	if h.MaxRetries < 0 {
		return fmt.Errorf("%s: max_retries must be positive", h.Name)
	}
	if err := h.normalizeSecrets(); err != nil {
		return err
	}
	if err := h.normalizeManifests(); err != nil {
		return err
	}
	if err := h.normalizeFiles(); err != nil {
		return err
	}
	if err := h.normalizeAction(); err != nil {
		return err
	}
	return h.normalizeOnFailure()
}

func (h *Hook) normalizeSecrets() error {
	for i, secret := range h.Secrets {
		name := strings.TrimPrefix(secret, "secret:")
		if name == "" {
			return fmt.Errorf("%s: secrets[%d] must not be empty", h.Name, i)
		}
		h.Secrets[i] = name
	}
	return nil
}

func (h *Hook) normalizeManifests() error {
	for i, manifest := range h.Manifests {
		cleaned, err := ValidateRelPath(manifest)
		if err != nil {
			return fmt.Errorf("%s: manifests[%d]: %q %w", h.Name, i, manifest, err)
		}
		h.Manifests[i] = cleaned
	}
	return nil
}

func (h *Hook) normalizeFiles() error {
	for i, file := range h.Files {
		cleaned, err := ValidateRelPath(file)
		if err != nil {
			return fmt.Errorf("%s: files[%d]: %w", h.Name, i, err)
		}
		h.Files[i] = cleaned
	}
	return nil
}

func (h *Hook) normalizeAction() error {
	switch h.Action {
	case HookActionHTTP:
		return h.normalizeHTTPAction()
	case HookActionSignal:
		return h.normalizeSignalAction()
	case HookActionRestart:
		return h.normalizeRestartAction()
	default:
		return fmt.Errorf("%s: action must be one of http, signal, restart", h.Name)
	}
}

func (h *Hook) normalizeHTTPAction() error {
	if field, ok := firstSetField(
		hookField{name: "container", value: h.Container},
		hookField{name: "signal", value: h.Signal},
	); ok {
		return fmt.Errorf("%s: %s cannot be set for http hooks", h.Name, field)
	}
	h.Method = cmp.Or(h.Method, http.MethodPost)
	if h.Method != http.MethodGet && h.Method != http.MethodPost {
		return fmt.Errorf("%s: method must be GET or POST", h.Name)
	}
	if h.URL == "" {
		return fmt.Errorf("%s: url is required for http hooks", h.Name)
	}
	if err := validateHookHTTPURL(h.Name, "url", h.URL); err != nil {
		return err
	}
	if h.HealthURL != "" {
		if err := validateHookHTTPURL(h.Name, "health_url", h.HealthURL); err != nil {
			return err
		}
	}
	return nil
}

func validateHookHTTPURL(hookName, field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %s is not a valid URL: %w", hookName, field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: %s must use http or https scheme", hookName, field)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: %s must include an explicit host", hookName, field)
	}
	return nil
}

func (h *Hook) normalizeSignalAction() error {
	if field, ok := firstSetField(
		hookField{name: "method", value: h.Method},
		hookField{name: "url", value: h.URL},
		hookField{name: "health_url", value: h.HealthURL},
	); ok {
		return fmt.Errorf("%s: %s cannot be set for signal hooks", h.Name, field)
	}
	if h.Container == "" {
		return fmt.Errorf("%s: container is required for signal hooks", h.Name)
	}
	h.Signal = cmp.Or(h.Signal, "HUP")
	return nil
}

func (h *Hook) normalizeRestartAction() error {
	if field, ok := firstSetField(
		hookField{name: "method", value: h.Method},
		hookField{name: "url", value: h.URL},
		hookField{name: "health_url", value: h.HealthURL},
		hookField{name: "container", value: h.Container},
		hookField{name: "signal", value: h.Signal},
	); ok {
		return fmt.Errorf("%s: %s cannot be set for restart hooks", h.Name, field)
	}
	return nil
}

func firstSetField(fields ...hookField) (string, bool) {
	for _, field := range fields {
		if field.value != "" {
			return field.name, true
		}
	}
	return "", false
}

func (h *Hook) normalizeOnFailure() error {
	switch h.OnFailure {
	case HookOnFailureKeepRunning, HookOnFailureRestart:
		return nil
	default:
		return fmt.Errorf("%s: on_failure must be keep_running or restart", h.Name)
	}
}
