package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
)

type CreateConfig struct {
	Hostname      string
	FleetDir      string
	Service       string
	TargetPath    string
	Script        bool
	SSH           string
	SkipGitChecks bool
	Stdout        io.Writer
	Stderr        io.Writer
}

func Create(ctx context.Context, cfg CreateConfig) error {
	cfg = normalizeCreateConfig(cfg)
	if !cfg.SkipGitChecks {
		if err := checkGitClean(ctx, cfg.FleetDir, cfg.Stderr); err != nil {
			return err
		}
	}
	data, err := buildCreateScriptData(ctx, cfg)
	if err != nil {
		return err
	}
	if cfg.SSH != "" {
		remote, err := renderCreateOutput("remote", data)
		if err != nil {
			return err
		}
		return runSSHCreate(ctx, cfg, remote)
	}
	variant := "plan"
	if cfg.Script {
		variant = "script"
	}
	out, err := renderCreateOutput(variant, data)
	if err != nil {
		return err
	}
	_, err = io.WriteString(cfg.Stdout, out)
	return err
}

// buildCreateScriptData resolves the host's picolet bundle (with placeholder
// secrets) and derives everything the rendered script needs from it.
func buildCreateScriptData(ctx context.Context, cfg CreateConfig) (createScriptData, error) {
	fleetCfg, err := config.LoadAll(os.DirFS(cfg.FleetDir))
	if err != nil {
		return createScriptData{}, fmt.Errorf("loading config: %w", err)
	}
	host, ok := fleetCfg.FindHost(cfg.Hostname)
	if !ok {
		return createScriptData{}, fmt.Errorf("host not found: %s", cfg.Hostname)
	}
	service := cfg.Service
	if service == "" {
		service, err = detectPicoletService(fleetCfg, host)
		if err != nil {
			return createScriptData{}, err
		}
	}

	resolved, err := resolveBootstrapHost(ctx, resolveConfig{
		RepoDir:    cfg.FleetDir,
		Config:     fleetCfg,
		Hostname:   cfg.Hostname,
		Service:    service,
		Rootless:   false,
		DataDir:    "/var/lib/picolet",
		SecretsDir: defaultSecretsDir,
		FileMode:   fileReaderPlaceholder,
	})
	if err != nil {
		return createScriptData{}, err
	}
	configFile, err := configSecret(resolved.Files, service+".service")
	if err != nil {
		return createScriptData{}, err
	}
	agentCfg, err := agentcfg.Parse([]byte(configFile.Content))
	if err != nil {
		return createScriptData{}, fmt.Errorf("parsing rendered picolet config: %w", err)
	}

	return createScriptData{
		Hostname:   cfg.Hostname,
		FleetDir:   cfg.FleetDir,
		TargetPath: cfg.TargetPath,
		Service:    service,
		Image:      fleetCfg.Fleet.Images["picolet"],
		Rootless:   service == "picolet",
		Secrets:    secretChecklist(agentCfg),
	}, nil
}

func normalizeCreateConfig(cfg CreateConfig) CreateConfig {
	if cfg.FleetDir == "" {
		cfg.FleetDir = "."
	}
	if cfg.TargetPath == "" {
		cfg.TargetPath = "/tmp/fleet"
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	return cfg
}

func detectPicoletService(fleetCfg *config.Config, host *config.HostConfig) (string, error) {
	services := fleetCfg.Assignments.Resolve(host).Services
	var found []string
	for _, service := range services {
		if service == "picolet" || service == "picolet-system" {
			found = append(found, service)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("host %s has no picolet service assigned (available: %s)", host.Hostname, strings.Join(services, ", "))
	default:
		return "", fmt.Errorf("host %s has multiple picolet services assigned: %s", host.Hostname, strings.Join(found, ", "))
	}
}

type createScriptData struct {
	Hostname   string
	FleetDir   string
	TargetPath string
	Service    string
	Image      string
	Rootless   bool
	Secrets    []string
}

// createTemplates renders the three output variants of `bootstrap create`:
// "plan" (annotated, for humans), "script" (bare runnable script), and
// "remote" (piped to `ssh ... bash -s` after rsync ran locally — it must
// contain nothing but the podman command, and in particular no follow-mode
// journalctl that would keep the remote shell alive forever).
const createTemplates = `
{{- define "rsync" -}}
rsync -a --delete {{q .FleetDir}}/ {{.Hostname}}:{{q .TargetPath}}/
{{- end -}}

{{- define "podman" -}}
{{if .Rootless}}podman run --rm \
  -v {{q .TargetPath}}:/repo:ro \
  -v $HOME/.config/picolet:/etc/picolet \
  -v $HOME/.local/share/picolet:/var/lib/picolet \
  -v $HOME/.config/containers/systemd:/etc/containers/systemd \
  -v $HOME/.config/systemd/user:/etc/systemd/system \
  -v $XDG_RUNTIME_DIR/systemd:$XDG_RUNTIME_DIR/systemd \
  -v $XDG_RUNTIME_DIR/podman/podman.sock:/run/podman/podman.sock \
  -e XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR \
{{- else}}sudo podman run --rm \
  -v {{q .TargetPath}}:/repo:ro \
  -v /etc/picolet:/etc/picolet \
  -v /var/lib/picolet-system:/var/lib/picolet \
  -v /etc/containers/systemd:/etc/containers/systemd \
  -v /etc/systemd/system:/etc/systemd/system \
  -v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket \
  -v /run/podman/podman.sock:/run/podman/podman.sock \
  --security-opt apparmor=unconfined \
{{- end}}
  --network host \
  {{q .Image}} bootstrap \
    --hostname={{q .Hostname}} \
    --repo-dir=/repo \
    --service={{q .Service}} \
    --systemd={{if .Rootless}}user{{else}}system{{end}}
{{- end -}}

{{- define "plan" -}}
# Bootstrap plan for {{.Hostname}} ({{if .Rootless}}rootless{{else}}rootful{{end}})
# Fleet repo: {{.FleetDir}}
# Picolet image: {{.Image}}
# Service: {{.Service}}

# Step 1 - Transfer the fleet repo to the target:
{{template "rsync" .}}

# Step 2 - Place host-managed secrets on the target:
{{- range .Secrets}}
#   {{.}}
{{- end}}

# Step 3 - Run bootstrap on the target:
{{template "podman" .}}

# Watch:
#   {{if not .Rootless}}sudo {{end}}journalctl {{if .Rootless}}--user {{end}}-fu {{.Service}}.service
{{end -}}

{{- define "script" -}}
set -euo pipefail
{{template "rsync" .}}
{{template "podman" .}}
{{end -}}

{{- define "remote" -}}
set -euo pipefail
{{template "podman" .}}
{{end -}}
`

var createTemplate = template.Must(
	template.New("create").Funcs(template.FuncMap{"q": shellQuote}).Parse(createTemplates),
)

func renderCreateOutput(variant string, data createScriptData) (string, error) {
	var b strings.Builder
	if err := createTemplate.ExecuteTemplate(&b, variant, data); err != nil {
		return "", fmt.Errorf("rendering bootstrap %s: %w", variant, err)
	}
	return b.String(), nil
}

func secretChecklist(cfg *agentcfg.Config) []string {
	var out []string
	if cfg.GitTokenPath != "" {
		out = append(out, cfg.GitTokenPath+" (required: git_token_path)")
	}
	if cfg.OnePassword != nil && cfg.OnePassword.TokenPath != "" {
		out = append(out, cfg.OnePassword.TokenPath+" (required: onepassword.token_path)")
	}
	if cfg.ProtonPass != nil && cfg.ProtonPass.PATPath != "" {
		out = append(out, cfg.ProtonPass.PATPath+" (required: protonpass.pat_path)")
	}
	if cfg.GitHubPrivateKeyPath != "" {
		out = append(out, cfg.GitHubPrivateKeyPath+" (required: github_private_key_path)")
	}
	return out
}

func runSSHCreate(ctx context.Context, cfg CreateConfig, remoteScript string) error {
	if err := runCmd(ctx, cfg.Stderr, "rsync", "-a", "--delete", withSlash(cfg.FleetDir), cfg.SSH+":"+withSlash(cfg.TargetPath)); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", cfg.SSH, "bash", "-s") //nolint:gosec // --ssh intentionally delegates to the operator's SSH target.
	cmd.Stdin = strings.NewReader(remoteScript)
	cmd.Stdout = cfg.Stdout
	cmd.Stderr = cfg.Stderr
	return cmd.Run()
}

func checkGitClean(ctx context.Context, dir string, stderr io.Writer) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		fmt.Fprintf(stderr, "warning: %s is not a git checkout; skipping clone-state checks\n", dir)
		return nil
	}
	status, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("fleet checkout has uncommitted changes; commit/stash them or pass --skip-git-checks")
	}
	if _, err := gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err != nil {
		fmt.Fprintf(stderr, "warning: %s has no upstream; skipping ahead/behind checks\n", dir)
		return nil
	}
	ahead, err := gitOutput(ctx, dir, "rev-list", "--count", "@{upstream}..HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(ahead) != "0" {
		return fmt.Errorf("fleet checkout has unpushed commits; push them or pass --skip-git-checks")
	}
	behind, err := gitOutput(ctx, dir, "rev-list", "--count", "HEAD..@{upstream}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(behind) != "0" {
		return fmt.Errorf("fleet checkout is behind its upstream; pull it or pass --skip-git-checks")
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func runCmd(ctx context.Context, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	return cmd.Run()
}

func withSlash(s string) string {
	return strings.TrimRight(s, "/") + "/"
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
