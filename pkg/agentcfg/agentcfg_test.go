package agentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

//nolint:funlen // table-driven test
func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Config
		wantErr string
	}{
		{
			name: "full config",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
repo_branch: develop
git_token_path: /etc/picolet/git-token
poll_interval: 30s
metrics_port: 9418
`,
			//nolint:gosec // G101: test fixture, not real credentials
			want: Config{
				Hostname:     "rpi5-1",
				RepoURL:      "https://github.com/example/fleet.git",
				RepoBranch:   "develop",
				GitTokenPath: "/etc/picolet/git-token",
				PollInterval: 30 * time.Second,
				MetricsPort:  9418,
			},
		},
		{
			name: "defaults applied",
			content: `
hostname: rpi5-1
repo_url: https://github.com/example/fleet.git
`,
			want: Config{
				Hostname:     "rpi5-1",
				RepoURL:      "https://github.com/example/fleet.git",
				RepoBranch:   "main",
				PollInterval: 60 * time.Second,
				MetricsPort:  9417,
			},
		},
		{
			name:    "missing hostname",
			content: "repo_url: https://example.com/repo.git\n",
			wantErr: "hostname is required",
		},
		{
			name:    "missing repo_url",
			content: "hostname: rpi5-1\n",
			wantErr: "repo_url is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := Load(path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if *got != tt.want {
				t.Errorf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/config.yml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
