// Package machine plans and runs `picolet bootstrap machine`: bringing up
// every Host a Fleet declares on one Machine. New is the planner (Fleet +
// Machine + options → ordered steps with stable ids, writing nothing);
// Evaluate annotates each
// step through the read side of HostOps and Render prints the result; Run
// checks each step the same way and applies it through the write side.
package machine

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
)

// Phase groups steps; the plan is phase-major, so every Host finishes a phase
// before any Host starts the next one.
type Phase int

const (
	// PhaseSetup brings each Host's user, session and directories into place.
	PhaseSetup Phase = iota + 1
	// PhaseCredentials places each Host's credential files.
	PhaseCredentials
	// PhaseHostBootstrap runs the containerized per-Host bootstrap.
	PhaseHostBootstrap
)

// Phases lists every phase in execution order.
var Phases = []Phase{PhaseSetup, PhaseCredentials, PhaseHostBootstrap}

func (p Phase) String() string {
	switch p {
	case PhaseSetup:
		return "users, sessions and directories"
	case PhaseCredentials:
		return "credential files"
	case PhaseHostBootstrap:
		return "per-Host bootstrap"
	}
	return fmt.Sprintf("phase %d", int(p))
}

// StepKind says what a step brings about; it selects the step's check.
type StepKind int

const (
	// StepUser: the Host's Linux user exists.
	StepUser StepKind = iota + 1
	// StepSubIDs: the user has subuid and subgid ranges.
	StepSubIDs
	// StepLinger: lingering is enabled for the user.
	StepLinger
	// StepUserManager: the user's systemd manager is running.
	StepUserManager
	// StepPodmanSocket: podman.socket is enabled and running (the user's, or
	// the system's for the rootful Host).
	StepPodmanSocket
	// StepDir: a directory the Agent quadlet bind-mounts, or a subdirectory
	// of credential files, exists with the Host's owner and Mode.
	StepDir
	// StepCheckout: the Fleet checkout is readable by the Host's user.
	StepCheckout
	// StepCredential: a credential file in the Host's secrets directory
	// holds Content, with the Host's owner and Mode.
	StepCredential
	// StepHostBootstrap: the containerized per-Host bootstrap has run.
	StepHostBootstrap
)

// podmanSocket is the unit the Agent reaches Podman through.
const podmanSocket = "podman.socket"

// Host is one Host of the planned Machine.
type Host struct {
	Hostname string
	// User is the Linux user the Agent runs as; empty for the rootful Host.
	User string
	// ListenPort is the port the Fleet declares for the Agent.
	ListenPort int
	// Bootstrap is the files the Host gets from Secret References instead
	// of the Machine's provider token; nil for a Host that runs with the
	// provider, empty for one declaring bootstrap: {} (neither).
	Bootstrap config.BootstrapFiles
}

// Rootful reports whether the Host's Agent runs as root.
func (h Host) Rootful() bool {
	return h.User == ""
}

// Step is one idempotent unit of work. It is data only: Evaluate derives the
// check from Kind.
type Step struct {
	// ID is stable across runs: <hostname>/<name>.
	ID    string
	Phase Phase
	Kind  StepKind
	Host  Host
	// Path is the directory of a StepDir or the file of a StepCredential
	// (relative to the user's home for a rootless Host, absolute for the
	// rootful Host), or the checkout of a StepCheckout.
	Path string
	// Mode is the permission of a StepDir or a StepCredential.
	Mode fs.FileMode
	// Content is what a StepCredential's file holds: a credential value,
	// never printed.
	Content []byte
	// Source is where a StepCredential's content comes from beyond
	// --secrets-dir: the provider token's flag or the Secret Reference.
	Source string
}

// Plan is the ordered work for one Machine.
type Plan struct {
	Machine string
	RepoDir string
	// Sources are the credential sources the operator gave, as shown.
	Sources []string
	// Hosts are the Machine's Hosts, sorted by hostname.
	Hosts []Host
	// Steps are in execution order.
	Steps []Step
	// Warnings are what the operator should know before the steps run but
	// what stops nothing.
	Warnings []string
	// Unresolved lists, as <hostname>/<file>, the bootstrap: files not
	// placed because their reference was not resolved.
	Unresolved []string
}

// Options are the operator's inputs beyond the Fleet.
type Options struct {
	// RepoDir is the absolute path of the Fleet checkout on the Machine.
	RepoDir string
	// SecretsDir holds each Host's credential files below <hostname>/;
	// nil when the operator gave none.
	SecretsDir *SecretsDir
	// Token is the Machine's provider token, placed for every Host without
	// bootstrap: files; nil when the operator gave none.
	Token *ProviderToken
	// Refs holds the values of the Hosts' bootstrap: references.
	Refs Resolved
}

// SecretsDir is the operator's --secrets-dir: <dir>/<hostname>/<file> is a
// credential file of the Host.
type SecretsDir struct {
	// Path is the directory, as shown to the operator.
	Path string
	// FS reads the directory. It must keep symlinks from leading out of it
	// (an os.Root's FS does): New reads it as root and follows symlinks.
	FS fs.FS
}

// agentDir is a directory the Agent quadlet bind-mounts, at its path for a
// rootless Host (relative to the home) and for the rootful Host.
type agentDir struct {
	name     string
	userPath string
	rootPath string
	mode     fs.FileMode
}

// secretsAgentDir is the Host's secrets directory: the Fleet convention the
// reference quadlets bind-mount to /etc/picolet/secrets, not derived from
// their Volume= lines.
var secretsAgentDir = agentDir{name: "secrets", userPath: ".config/picolet/secrets", rootPath: "/etc/picolet/secrets", mode: 0o700}

var agentDirs = []agentDir{
	secretsAgentDir,
	{name: "data", userPath: ".local/share/picolet", rootPath: "/var/lib/picolet-system", mode: 0o700},
	{name: "quadlets", userPath: ".config/containers/systemd", rootPath: "/etc/containers/systemd", mode: 0o755},
	{name: "units", userPath: ".config/systemd/user", rootPath: "/etc/systemd/system", mode: 0o755},
}

// path is d's path for h: relative to the home for a rootless Host,
// absolute for the rootful Host.
func (d agentDir) path(h Host) string {
	if h.Rootful() {
		return d.rootPath
	}
	return d.userPath
}

// New plans the bootstrap of machine: every Host of the Fleet declaring it,
// phase by phase. It reads the Hosts' credential files from opts.SecretsDir
// and writes nothing. It errors when no Host runs on machine, and when two
// sources place the same credential file.
func New(cfg *config.Config, machine string, opts Options) (*Plan, error) {
	hosts, err := machineHosts(cfg, machine)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Machine: machine, RepoDir: opts.RepoDir, Hosts: hosts}
	if opts.SecretsDir != nil {
		plan.Sources = append(plan.Sources, "--secrets-dir "+opts.SecretsDir.Path)
	}
	if opts.Token != nil {
		plan.Sources = append(plan.Sources, opts.Token.provider.flag+" "+opts.Token.Path)
	}
	if opts.Refs.Warning != "" {
		plan.Warnings = append(plan.Warnings, opts.Refs.Warning)
	}
	for _, h := range hosts {
		plan.Steps = append(plan.Steps, setupSteps(h, opts.RepoDir)...)
	}
	for _, h := range hosts {
		steps, err := hostCredentialSteps(plan, h, opts)
		if err != nil {
			return nil, err
		}
		plan.Steps = append(plan.Steps, steps...)
	}
	for _, h := range hosts {
		plan.Steps = append(plan.Steps, Step{ID: h.Hostname + "/bootstrap", Phase: PhaseHostBootstrap, Kind: StepHostBootstrap, Host: h})
	}
	return plan, nil
}

// hostCredentialSteps plans h's credential files, in order: its
// --secrets-dir files, the Machine's provider token for a Host without a
// bootstrap: block, and the block's files whose reference was resolved. It
// records on plan the warnings and the files not placed.
func hostCredentialSteps(plan *Plan, h Host, opts Options) ([]Step, error) {
	steps, warning, err := credentialSteps(h, opts.SecretsDir)
	if err != nil {
		return nil, err
	}
	if warning != "" {
		plan.Warnings = append(plan.Warnings, warning)
	}
	if t := opts.Token; t != nil && h.Bootstrap == nil {
		steps = append(steps, credentialStep(h, t.provider.tokenName, t.Content, t.provider.flag))
	}
	// An unresolved file still claims its path: it collides with a
	// --secrets-dir file as a resolved one would.
	claims := slices.Clone(steps)
	for _, name := range slices.Sorted(maps.Keys(h.Bootstrap)) {
		ref := h.Bootstrap[name]
		value, ok := opts.Refs.Values[ref]
		if !ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: %s not placed: %s %s", h.Hostname, name, ref, opts.Refs.Missing[ref]))
			plan.Unresolved = append(plan.Unresolved, h.Hostname+"/"+name)
			claims = append(claims, credentialStep(h, name, nil, ref))
			continue
		}
		step := credentialStep(h, name, []byte(value), ref)
		steps = append(steps, step)
		claims = append(claims, step)
	}
	return steps, oneSourcePerPath(h, claims)
}

// oneSourcePerPath rejects two of h's credential steps placing one path.
func oneSourcePerPath(h Host, steps []Step) error {
	from := map[string]string{}
	for _, s := range steps {
		source := cmp.Or(s.Source, "--secrets-dir")
		if other, ok := from[s.Path]; ok {
			return fmt.Errorf("%s: %s comes from both %s and %s: give each credential file one source",
				h.Hostname, s.displayPath(), other, source)
		}
		from[s.Path] = source
	}
	return nil
}

func machineHosts(cfg *config.Config, machine string) ([]Host, error) {
	if machine == "" {
		return nil, errors.New("machine name is required")
	}
	key := config.MachineKey(machine)
	var hosts []Host
	known := map[string]string{}
	for _, h := range cfg.Hosts {
		known[config.MachineKey(h.Machine)] = h.Machine
		if config.MachineKey(h.Machine) == key {
			hosts = append(hosts, Host{Hostname: h.Hostname, User: h.User, ListenPort: h.ListenPort, Bootstrap: h.Bootstrap})
		}
	}
	if len(hosts) == 0 {
		names := make([]string, 0, len(known))
		for _, name := range known {
			names = append(names, name)
		}
		slices.Sort(names)
		return nil, fmt.Errorf("no Host in the Fleet runs on machine %q (machines: %s)", machine, strings.Join(names, ", "))
	}
	slices.SortFunc(hosts, func(a, b Host) int { return cmp.Compare(a.Hostname, b.Hostname) })
	return hosts, nil
}

func setupSteps(h Host, repoDir string) []Step {
	step := func(name string, kind StepKind) Step {
		return Step{ID: h.Hostname + "/" + name, Phase: PhaseSetup, Kind: kind, Host: h}
	}
	var steps []Step
	if !h.Rootful() {
		steps = append(steps,
			step("user", StepUser),
			step("subids", StepSubIDs),
			step("linger", StepLinger),
			step("user-manager", StepUserManager),
		)
	}
	steps = append(steps, step("podman-socket", StepPodmanSocket))
	for _, d := range agentDirs {
		s := step("dir/"+d.name, StepDir)
		s.Path, s.Mode = d.path(h), d.mode
		steps = append(steps, s)
	}
	if !h.Rootful() {
		s := step("checkout", StepCheckout)
		s.Path = repoDir
		steps = append(steps, s)
	}
	return steps
}

// Describe states the end state a step brings about.
func (s Step) Describe() string { //nolint:cyclop // one case per step kind
	switch s.Kind {
	case StepUser:
		return "Linux user " + s.Host.User + " present"
	case StepSubIDs:
		return "subuid/subgid ranges of " + s.Host.User + " present"
	case StepLinger:
		return "lingering enabled for " + s.Host.User
	case StepUserManager:
		return "user manager of " + s.Host.User + " running"
	case StepPodmanSocket:
		if s.Host.Rootful() {
			return "system " + podmanSocket + " enabled and running"
		}
		return "user " + podmanSocket + " of " + s.Host.User + " enabled and running"
	case StepDir:
		return fmt.Sprintf("directory %s, owner %s, mode %04o", s.displayPath(), s.Host.owner(), s.Mode)
	case StepCheckout:
		return "Fleet checkout readable by " + s.Host.User + " (world-readable)"
	case StepCredential:
		describe := fmt.Sprintf("credential file %s, owner %s, mode %04o", s.displayPath(), s.Host.owner(), s.Mode)
		if s.Source != "" {
			describe += ", from " + s.Source
		}
		return describe
	case StepHostBootstrap:
		if s.Host.Rootful() {
			return "per-Host bootstrap as root (--systemd system)"
		}
		return "per-Host bootstrap as " + s.Host.User + " (--systemd user)"
	}
	return s.ID
}

// displayPath is a StepDir's or StepCredential's path as ~user/... for a
// rootless Host: exact whatever the home turns out to be.
func (s Step) displayPath() string {
	if s.Host.Rootful() {
		return s.Path
	}
	return "~" + s.Host.User + "/" + s.Path
}

func (h Host) owner() string {
	if h.Rootful() {
		return "root"
	}
	return h.User
}
