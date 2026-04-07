package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRepoURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   string
	}{
		{
			name:      "https with .git suffix",
			url:       "https://github.com/drk-darmstadt-iuk/iuk-gitops.git",
			wantOwner: "drk-darmstadt-iuk",
			wantRepo:  "iuk-gitops",
		},
		{
			name:      "https without .git suffix",
			url:       "https://github.com/org/repo",
			wantOwner: "org",
			wantRepo:  "repo",
		},
		{
			name:    "ssh URL rejected",
			url:     "git@github.com:org/repo.git",
			wantErr: "GitHub App requires HTTPS",
		},
		{
			name:    "non-GitHub URL rejected",
			url:     "https://gitlab.com/org/repo.git",
			wantErr: "not a GitHub URL",
		},
		{
			name:    "http URL rejected",
			url:     "http://github.com/org/repo.git",
			wantErr: "requires HTTPS",
		},
		{
			name:    "empty URL",
			url:     "",
			wantErr: "empty",
		},
		{
			name:    "URL with too few path segments",
			url:     "https://github.com/org",
			wantErr: "expected owner/repo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := ParseRepoURL(tt.url)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}
