package validator

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// The Host sees the data dir at testHostDataDir while picolet writes to
// testDataDir (containerized Agent): references use the host-visible path.
const (
	testDataDir     = "/var/lib/picolet"
	testHostDataDir = "/srv/picolet"
)

// deliveredFile returns a files/ category file delivered at relPath.
func deliveredFile(relPath string) resolver.ResolvedFile {
	return resolver.ResolvedFile{
		SrcPath:  "services/app/files/" + relPath,
		DestPath: testDataDir + "/files/" + relPath,
		Content:  "FROM docker.io/library/alpine:3.23\n",
		Category: config.CategoryFile,
		RelPath:  relPath,
	}
}

//nolint:funlen // table-driven test
func TestAnalyzeFilesBuildReferencesDeliveredFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		build   string
		files   []resolver.ResolvedFile
		wantErr string
	}{
		{
			name:    "Containerfile the Fleet does not deliver",
			build:   "File=" + testHostDataDir + "/files/app/Containerfile\nSetWorkingDirectory=file\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/Containerfle")},
			wantErr: "app.build: File=" + testHostDataDir + "/files/app/Containerfile is not delivered by the Fleet",
		},
		{
			name:  "delivered Containerfile",
			build: "File=" + testHostDataDir + "/files/app/Containerfile\nSetWorkingDirectory=file\n",
			files: []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
		},
		{
			name:  "Containerfile the operator manages outside the delivered directories",
			build: "File=/opt/app/Containerfile\nSetWorkingDirectory=file\n",
		},
		{
			name:  "Containerfile in the data dir's unmanaged part (the repo clone)",
			build: "File=" + testHostDataDir + "/repo/app/Containerfile\nSetWorkingDirectory=file\n",
		},
		{
			name:  "Containerfile in a sibling directory sharing the files/ prefix",
			build: "File=" + testHostDataDir + "/files2/app/Containerfile\nSetWorkingDirectory=file\n",
		},
		{
			name:    "Containerfile in files/ while the Fleet delivers no data files",
			build:   "File=" + testHostDataDir + "/files/app/Containerfile\nSetWorkingDirectory=file\n",
			wantErr: "app.build: File=" + testHostDataDir + "/files/app/Containerfile is not delivered by the Fleet",
		},
		{
			name:  "delivered build context",
			build: "File=Containerfile\nSetWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files: []resolver.ResolvedFile{deliveredFile("app/Containerfile"), deliveredFile("app/src/main.go")},
		},
		{
			name:    "build context the Fleet delivers nothing to",
			build:   "File=/opt/app/Containerfile\nSetWorkingDirectory=" + testHostDataDir + "/files/ap\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
			wantErr: "app.build: SetWorkingDirectory=" + testHostDataDir + "/files/ap is not delivered by the Fleet",
		},
		{
			name:    "working directory the Fleet delivers nothing to",
			build:   "\n[Service]\nWorkingDirectory=" + testHostDataDir + "/files/ap\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
			wantErr: "app.build: WorkingDirectory=" + testHostDataDir + "/files/ap is not delivered by the Fleet",
		},
		{
			name:    "relative Containerfile the Fleet does not deliver to the working directory",
			build:   "File=Containerfile.prod\n\n[Service]\nWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
			wantErr: "app.build: File=Containerfile.prod (" + testHostDataDir + "/files/app/Containerfile.prod) is not delivered by the Fleet",
		},
		{
			// systemd fails to chdir into a missing WorkingDirectory=, even
			// when podman build uses the absolute Containerfile's directory.
			name:    "absolute Containerfile beside an undelivered working directory",
			build:   "File=" + testHostDataDir + "/files/app/Containerfile\nSetWorkingDirectory=file\n\n[Service]\nWorkingDirectory=" + testHostDataDir + "/files/ap\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
			wantErr: "app.build: WorkingDirectory=" + testHostDataDir + "/files/ap is not delivered by the Fleet",
		},
		{
			// Quadlet takes a File= starting with "http" for a URL and passes
			// no context, but podman build reads it locally.
			name:    "File= only Quadlet takes for a URL",
			build:   "File=http-app/Containerfile\n\n[Service]\nWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
			wantErr: "app.build: File=http-app/Containerfile (" + testHostDataDir + "/files/app/http-app/Containerfile) is not delivered by the Fleet",
		},
		{
			name:  "File= podman build fetches",
			build: "File=https://example.com/app/Containerfile\n\n[Service]\nWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files: []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
		},
		{
			name:    "SetWorkingDirectory=unit keeps an explicit working directory as the context",
			build:   "SetWorkingDirectory=unit\n\n[Service]\nWorkingDirectory=" + testHostDataDir + "/files/ap\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/Containerfile")},
			wantErr: "app.build: WorkingDirectory=" + testHostDataDir + "/files/ap is not delivered by the Fleet",
		},
		{
			// Podman looks for a relative File= in the working directory,
			// then in the context: either may hold it.
			name: "relative Containerfile with a context apart from the working directory",
			build: "File=Containerfile\nSetWorkingDirectory=" + testHostDataDir + "/files/app\n\n" +
				"[Service]\nWorkingDirectory=" + testHostDataDir + "/files/tools\n",
			files: []resolver.ResolvedFile{deliveredFile("app/Containerfile"), deliveredFile("tools/lint.sh")},
		},
		{
			name:  "URL build context",
			build: "SetWorkingDirectory=https://github.com/example/app.git\n",
		},
		{
			name:  "systemd specifier in the Containerfile path",
			build: "File=" + testHostDataDir + "/files/%i/Containerfile\nSetWorkingDirectory=file\n",
		},
		{
			name:  "build context relative to the unit file",
			build: "File=Containerfile\nSetWorkingDirectory=app\n",
		},
		{
			// Without [Service] WorkingDirectory=, podman build first looks in
			// the service's default directory (/ or the user's home), which
			// the operator may have put a Containerfile in.
			name:  "relative Containerfile the Fleet does not deliver to the build context",
			build: "File=Containerfile\nSetWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files: []resolver.ResolvedFile{deliveredFile("app/src/main.go")},
		},
		{
			name: "relative Containerfile in neither the working directory nor the context",
			build: "File=Containerfile\nSetWorkingDirectory=" + testHostDataDir + "/files/app\n\n" +
				"[Service]\nWorkingDirectory=" + testHostDataDir + "/files/tools\n",
			files: []resolver.ResolvedFile{deliveredFile("app/src/main.go"), deliveredFile("tools/lint.sh")},
			wantErr: "app.build: File=Containerfile (" + testHostDataDir + "/files/tools/Containerfile or " +
				testHostDataDir + "/files/app/Containerfile) is not delivered by the Fleet",
		},
		{
			name: "relative Containerfile missing from a context that is the working directory",
			build: "File=missing\nSetWorkingDirectory=" + testHostDataDir + "/files/app\n\n" +
				"[Service]\nWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/src/main.go")},
			wantErr: "app.build: File=missing (" + testHostDataDir + "/files/app/missing) is not delivered by the Fleet",
		},
		{
			// Podman drops a unit-relative SetWorkingDirectory= when
			// [Service] WorkingDirectory= is set; that becomes the context.
			name: "relative Containerfile missing from the working directory beside a unit-relative context",
			build: "File=missing\nSetWorkingDirectory=ignored\n\n" +
				"[Service]\nWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files:   []resolver.ResolvedFile{deliveredFile("app/src/main.go")},
			wantErr: "app.build: File=missing (" + testHostDataDir + "/files/app/missing) is not delivered by the Fleet",
		},
		{
			// podman build falls back to File= joined onto the context.
			name:  "absolute Containerfile delivered below the context",
			build: "File=" + testHostDataDir + "/files/missing/Containerfile\nSetWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files: []resolver.ResolvedFile{deliveredFile("app" + testHostDataDir + "/files/missing/Containerfile")},
		},
		{
			name:    "build context the Fleet delivers as a file",
			build:   "File=/opt/app/Containerfile\nSetWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files:   []resolver.ResolvedFile{deliveredFile("app")},
			wantErr: "app.build: SetWorkingDirectory=" + testHostDataDir + "/files/app is not delivered by the Fleet",
		},
		{
			name:    "working directory the Fleet delivers as a file",
			build:   "\n[Service]\nWorkingDirectory=" + testHostDataDir + "/files/app\n",
			files:   []resolver.ResolvedFile{deliveredFile("app")},
			wantErr: "app.build: WorkingDirectory=" + testHostDataDir + "/files/app is not delivered by the Fleet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			build := newParsedFile(t, config.CategoryBuild, testQuadletDir+"app.build",
				"[Build]\nImageTag=localhost/app:latest\n"+tt.build)
			files := append([]resolver.ResolvedFile{build}, tt.files...)
			_, err := AnalyzeFiles(files, Target{HostDataDir: testHostDataDir})
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}
}
