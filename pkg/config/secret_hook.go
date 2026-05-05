package config

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	HookActionHTTP    = "http"
	HookActionSignal  = "signal"
	HookActionRestart = "restart"

	HookOnFailureKeepRunning = "keep_running"
	HookOnFailureRestart     = "restart"
)

// SecretHook describes a runtime action to run after one or more Podman secrets change.
type SecretHook struct {
	Name      string   `yaml:"name"`
	Secrets   []string `yaml:"secrets"`
	Unit      string   `yaml:"unit"`
	Action    string   `yaml:"action"`
	Method    string   `yaml:"method"`
	URL       string   `yaml:"url"`
	HealthURL string   `yaml:"health_url"`
	Container string   `yaml:"container"`
	Signal    string   `yaml:"signal"`
	OnFailure string   `yaml:"on_failure"`
}

// SecretHooksFile is the service-bundle metadata file schema.
type SecretHooksFile struct {
	SecretHooks []SecretHook `yaml:"secret_hooks"`
}

// Normalize applies defaults and validates the hook.
func (h *SecretHook) Normalize() error {
	if h.Name == "" {
		return errors.New("name is required")
	}
	if len(h.Secrets) == 0 {
		return fmt.Errorf("%s: secrets must not be empty", h.Name)
	}
	if h.Unit == "" {
		return fmt.Errorf("%s: unit is required", h.Name)
	}
	if h.Action == "" {
		return fmt.Errorf("%s: action is required", h.Name)
	}
	if h.OnFailure == "" {
		h.OnFailure = HookOnFailureKeepRunning
	}
	for i, secret := range h.Secrets {
		name := strings.TrimPrefix(secret, "secret:")
		if name == "" {
			return fmt.Errorf("%s: secrets[%d] must not be empty", h.Name, i)
		}
		h.Secrets[i] = name
	}
	if err := h.normalizeAction(); err != nil {
		return err
	}
	return h.normalizeOnFailure()
}

func (h *SecretHook) normalizeAction() error {
	switch h.Action {
	case HookActionHTTP:
		return h.normalizeHTTPAction()
	case HookActionSignal:
		return h.normalizeSignalAction()
	case HookActionRestart:
		// unit is already required for every hook.
		return nil
	default:
		return fmt.Errorf("%s: action must be one of http, signal, restart", h.Name)
	}
}

func (h *SecretHook) normalizeHTTPAction() error {
	if h.Method == "" {
		h.Method = http.MethodPost
	}
	if h.Method != http.MethodGet && h.Method != http.MethodPost {
		return fmt.Errorf("%s: method must be GET or POST", h.Name)
	}
	if h.URL == "" {
		return fmt.Errorf("%s: url is required for http hooks", h.Name)
	}
	return nil
}

func (h *SecretHook) normalizeSignalAction() error {
	if h.Container == "" {
		return fmt.Errorf("%s: container is required for signal hooks", h.Name)
	}
	if h.Signal == "" {
		h.Signal = "HUP"
	}
	return nil
}

func (h *SecretHook) normalizeOnFailure() error {
	switch h.OnFailure {
	case HookOnFailureKeepRunning, HookOnFailureRestart:
		return nil
	default:
		return fmt.Errorf("%s: on_failure must be keep_running or restart", h.Name)
	}
}
