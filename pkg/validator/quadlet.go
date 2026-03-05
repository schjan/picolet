package validator

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
)

func parseQuadletUnit(filename, content string) (*parser.UnitFile, error) {
	unit := parser.NewUnitFile()
	unit.Filename = filename
	if err := unit.Parse(content); err != nil {
		return nil, err
	}
	return unit, nil
}

func unitServiceName(unit *parser.UnitFile) string {
	serviceName, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		return ""
	}
	return serviceName + ".service"
}

func (v *Validator) validateQuadlet(path string, content []byte, unitsInfoMap map[string]*quadlet.UnitInfo) error {
	ext := filepath.Ext(path)
	filename := filepath.Base(path)

	unit, err := parseQuadletUnit(filename, string(content))
	if err != nil {
		return fmt.Errorf("%s: parse: %w", path, err)
	}

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

// UnitNameFromContent returns the systemd unit name for a quadlet given its
// filename and content string, respecting any ServiceName= override.
// Returns empty string for non-quadlet filenames or unparseable content.
func UnitNameFromContent(filename, content string) string {
	unit, err := parseQuadletUnit(filename, content)
	if err != nil {
		return ""
	}
	return unitServiceName(unit)
}

// UnitNameFromFile returns the systemd unit name for a quadlet file on disk,
// respecting any ServiceName= override in the file content.
// Returns empty string if the file cannot be read or is not a quadlet.
func UnitNameFromFile(destPath string) string {
	unit, err := parser.ParseUnitFile(destPath)
	if err != nil {
		return ""
	}
	return unitServiceName(unit)
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
