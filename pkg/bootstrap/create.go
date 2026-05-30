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

func Create(ctx context.Context, cfg CreateConfig) error { //nolint:cyclop // sequential validation, resolve, render, optional ssh.
	cfg = normalizeCreateConfig(cfg)
	if !cfg.SkipGitChecks {
		if err := checkGitClean(ctx, cfg.FleetDir, cfg.Stderr); err != nil {
			return err
		}
	}
	repoFS := os.DirFS(cfg.FleetDir)
	fleetCfg, err := config.LoadAll(repoFS)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	host, ok := fleetCfg.FindHost(cfg.Hostname)
	if !ok {
		return fmt.Errorf("host not found: %s", cfg.Hostname)
	}
	service := cfg.Service
	if service == "" {
		service, err = detectPicoletService(fleetCfg, host)
		if err != nil {
			return err
		}
	}
	rootless := service == "picolet"

	resolved, err := resolveBootstrapHost(ctx, resolveConfig{
		RepoDir:    cfg.FleetDir,
		Hostname:   cfg.Hostname,
		Service:    service,
		Rootless:   false,
		DataDir:    "/var/lib/picolet",
		SecretsDir: defaultSecretsDir,
		FileMode:   fileReaderPlaceholder,
	})
	if err != nil {
		return err
	}
	configFile, err := configSecret(resolved.Files)
	if err != nil {
		return err
	}
	agentCfg, err := agentcfg.Parse([]byte(configFile.Content))
	if err != nil {
		return fmt.Errorf("parsing rendered picolet config: %w", err)
	}

	script := renderCreateScript(createScriptData{
		Hostname:   cfg.Hostname,
		FleetDir:   cfg.FleetDir,
		TargetPath: cfg.TargetPath,
		Service:    service,
		Image:      fleetCfg.Fleet.Images["picolet"],
		Rootless:   rootless,
		Script:     cfg.Script,
		Secrets:    secretChecklist(agentCfg),
	})
	if cfg.SSH != "" {
		return runSSHCreate(ctx, cfg, script)
	}
	_, err = io.WriteString(cfg.Stdout, script)
	return err
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
	Script     bool
	Secrets    []string
}

func renderCreateScript(data createScriptData) string {
	var b strings.Builder
	if data.Script {
		fmt.Fprintf(&b, "set -euo pipefail\n")
	} else {
		fmt.Fprintf(&b, "# Bootstrap plan for %s (%s)\n", data.Hostname, modeLabel(data.Rootless))
		fmt.Fprintf(&b, "# Fleet repo: %s\n", data.FleetDir)
		fmt.Fprintf(&b, "# Picolet image: %s\n", data.Image)
		fmt.Fprintf(&b, "# Service: %s\n\n", data.Service)
		fmt.Fprintf(&b, "# Step 1 - Transfer the fleet repo to the target:\n")
	}
	fmt.Fprintf(&b, "rsync -a --delete %s/ %s:%s/\n\n", shellQuote(data.FleetDir), data.Hostname, shellQuote(data.TargetPath))
	if !data.Script {
		fmt.Fprintf(&b, "# Step 2 - Place host-managed secrets on the target:\n")
	}
	for _, secret := range data.Secrets {
		if data.Script {
			continue
		}
		fmt.Fprintf(&b, "#   %s\n", secret)
	}
	if !data.Script {
		fmt.Fprintf(&b, "\n# Step 3 - Run bootstrap on the target:\n")
	}
	writePodmanCommand(&b, data)
	if !data.Script {
		fmt.Fprintf(&b, "\n# Watch:\n%sjournalctl %s-fu %s.service\n", sudoPrefix(data.Rootless), userFlag(data.Rootless), data.Service)
	}
	return b.String()
}

func writePodmanCommand(b *strings.Builder, data createScriptData) {
	if !data.Rootless {
		fmt.Fprintf(b, "sudo podman run --rm \\\n")
		fmt.Fprintf(b, "  -v %s:/repo:ro \\\n", shellQuote(data.TargetPath))
		fmt.Fprintf(b, "  -v /etc/picolet:/etc/picolet \\\n")
		fmt.Fprintf(b, "  -v /var/lib/picolet-system:/var/lib/picolet \\\n")
		fmt.Fprintf(b, "  -v /etc/containers/systemd:/etc/containers/systemd \\\n")
		fmt.Fprintf(b, "  -v /etc/systemd/system:/etc/systemd/system \\\n")
		fmt.Fprintf(b, "  -v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket \\\n")
		fmt.Fprintf(b, "  -v /run/podman/podman.sock:/run/podman/podman.sock \\\n")
		fmt.Fprintf(b, "  --security-opt apparmor=unconfined \\\n")
		fmt.Fprintf(b, "  --network host \\\n")
	} else {
		fmt.Fprintf(b, "podman run --rm \\\n")
		fmt.Fprintf(b, "  -v %s:/repo:ro \\\n", shellQuote(data.TargetPath))
		fmt.Fprintf(b, "  -v $HOME/.config/picolet:/etc/picolet \\\n")
		fmt.Fprintf(b, "  -v $HOME/.local/share/picolet:/var/lib/picolet \\\n")
		fmt.Fprintf(b, "  -v $HOME/.config/containers/systemd:/etc/containers/systemd \\\n")
		fmt.Fprintf(b, "  -v $HOME/.config/systemd/user:/etc/systemd/system \\\n")
		fmt.Fprintf(b, "  -v $XDG_RUNTIME_DIR/systemd:$XDG_RUNTIME_DIR/systemd \\\n")
		fmt.Fprintf(b, "  -v $XDG_RUNTIME_DIR/podman/podman.sock:/run/podman/podman.sock \\\n")
		fmt.Fprintf(b, "  -e XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR \\\n")
		fmt.Fprintf(b, "  --network host \\\n")
	}
	fmt.Fprintf(b, "  %s bootstrap \\\n", shellQuote(data.Image))
	fmt.Fprintf(b, "    --hostname=%s \\\n", shellQuote(data.Hostname))
	fmt.Fprintf(b, "    --repo-dir=/repo \\\n")
	fmt.Fprintf(b, "    --service=%s", shellQuote(data.Service))
	if data.Rootless {
		fmt.Fprintf(b, " \\\n    --systemd=user")
	} else {
		fmt.Fprintf(b, " \\\n    --systemd=system")
	}
	fmt.Fprintf(b, "\n")
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

func runSSHCreate(ctx context.Context, cfg CreateConfig, script string) error {
	if err := runCmd(ctx, cfg.Stderr, "rsync", "-a", "--delete", withSlash(cfg.FleetDir), cfg.SSH+":"+withSlash(cfg.TargetPath)); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", cfg.SSH, "bash", "-s") //nolint:gosec // --ssh intentionally delegates to the operator's SSH target.
	cmd.Stdin = strings.NewReader(remoteBootstrapScript(script))
	cmd.Stdout = cfg.Stdout
	cmd.Stderr = cfg.Stderr
	return cmd.Run()
}

func remoteBootstrapScript(script string) string {
	for _, marker := range []string{"sudo podman run", "podman run"} {
		if i := strings.Index(script, marker); i >= 0 {
			return "set -euo pipefail\n" + script[i:]
		}
	}
	return "set -euo pipefail\n" + script
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

func modeLabel(rootless bool) string {
	if rootless {
		return "rootless"
	}
	return "rootful"
}

func sudoPrefix(rootless bool) string {
	if rootless {
		return ""
	}
	return "sudo "
}

func userFlag(rootless bool) string {
	if rootless {
		return "--user "
	}
	return ""
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
