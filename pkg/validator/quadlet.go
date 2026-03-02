package validator

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
)

func (v *Validator) validateQuadlet(path string, content []byte) error {
	ext := filepath.Ext(path)
	filename := filepath.Base(path)

	unit := parser.NewUnitFile()
	unit.Filename = filename
	if err := unit.Parse(string(content)); err != nil {
		return fmt.Errorf("%s: parse: %w", path, err)
	}

	unitsInfoMap := v.unitsInfoMap()
	// Ensure the unit being validated is in the map (required by Podman's converter)
	if _, ok := unitsInfoMap[filename]; !ok {
		unitsInfoMap[filename] = BuildUnitInfo(filename, ext)
	}

	var warn error
	var convertErr error

	switch ext {
	case ".container":
		_, warn, convertErr = quadlet.ConvertContainer(unit, unitsInfoMap, false)
	case ".network":
		_, warn, convertErr = quadlet.ConvertNetwork(unit, unitsInfoMap, false)
	case ".volume":
		_, warn, convertErr = quadlet.ConvertVolume(unit, unitsInfoMap, false)
	case ".kube":
		_, convertErr = quadlet.ConvertKube(unit, unitsInfoMap, false)
	default:
		return fmt.Errorf("%s: unknown quadlet extension %q", path, ext)
	}

	if warn != nil {
		slog.Warn("quadlet warning", "file", path, "warning", warn)
	}
	if convertErr != nil {
		return fmt.Errorf("%s: %w", path, convertErr)
	}
	return nil
}

// unitsInfoMap builds the UnitInfo map from all resolved files for the current host.
// This allows Podman's converter to resolve cross-references between units
// (e.g., a .container referencing a .network or .volume).
func (v *Validator) unitsInfoMap() map[string]*quadlet.UnitInfo {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.unitsInfo != nil {
		return v.unitsInfo
	}

	v.unitsInfo = make(map[string]*quadlet.UnitInfo)
	for _, f := range v.currentFiles {
		ext := filepath.Ext(f.DestPath)
		filename := filepath.Base(f.DestPath)
		if info := BuildUnitInfo(filename, ext); info != nil {
			v.unitsInfo[filename] = info
		}
	}
	return v.unitsInfo
}

// UnitNameFromPath returns the systemd unit name for a quadlet destination path.
// For example, "/etc/containers/systemd/foo.container" → "foo.service".
// Returns empty string for non-quadlet paths.
func UnitNameFromPath(destPath string) string {
	filename := filepath.Base(destPath)
	ext := filepath.Ext(destPath)
	info := BuildUnitInfo(filename, ext)
	if info == nil {
		return ""
	}
	return info.ServiceName
}

// BuildUnitInfo returns the UnitInfo for a quadlet file, mapping filename + extension
// to systemd service name and Podman resource name. Returns nil for unknown extensions.
func BuildUnitInfo(filename, ext string) *quadlet.UnitInfo {
	baseName := strings.TrimSuffix(filename, ext)
	switch ext {
	case ".container":
		return &quadlet.UnitInfo{
			ServiceName:  baseName + ".service",
			ResourceName: baseName,
		}
	case ".network":
		return &quadlet.UnitInfo{
			ServiceName:  baseName + "-network.service",
			ResourceName: "systemd-" + baseName,
		}
	case ".volume":
		return &quadlet.UnitInfo{
			ServiceName:  baseName + "-volume.service",
			ResourceName: "systemd-" + baseName,
		}
	case ".kube":
		return &quadlet.UnitInfo{
			ServiceName:  baseName + ".service",
			ResourceName: baseName,
		}
	}
	return nil
}
