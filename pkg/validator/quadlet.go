package validator

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
)

// buildUnitInfo mirrors Podman's generateUnitsInfoMap logic exactly.
// GetUnitServiceName returns the base service name without ".service" suffix;
// quadlet.UnitInfo.ServiceFileName appends the suffix when needed.
// ResourceName must be pre-filled for .container (network reuse resolution via GetContainerResourceName).
// Convert* fills ResourceName for all other types (.network, .volume, .kube).
func buildUnitInfo(unit *parser.UnitFile) *quadlet.UnitInfo {
	serviceName, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		return nil
	}
	info := &quadlet.UnitInfo{ServiceName: serviceName}
	if strings.HasSuffix(unit.Filename, ".container") {
		info.ResourceName = quadlet.GetContainerResourceName(unit)
	}
	// .network, .volume, .kube: ResourceName left empty — Convert* sets it
	return info
}

// convertQuadlet validates a pre-parsed quadlet unit against the units info map
// by running Podman's Convert* for its type, returning the generated service unit.
// The unit must already be parsed by the caller (via ValidateFiles or validateFile).
// Podman's Convert* functions require the unit's own entry in unitsInfoMap
// (via initServiceUnitFile), so we ensure it is populated before converting.
// rootless must be true when validating units for rootless Podman; it controls
// systemd dependency generation (e.g. podman-user-wait-network-online.service vs
// network-online.target).
func convertQuadlet(unit *parser.UnitFile, unitsInfoMap map[string]*quadlet.UnitInfo, rootless bool) (*parser.UnitFile, error) {
	// Ensure the unit's own entry is in the map. In the ValidateFiles path this is
	// pre-populated by buildUnitsInfoFromFiles; this handles direct calls (e.g. tests).
	if _, ok := unitsInfoMap[unit.Filename]; !ok {
		if info := buildUnitInfo(unit); info != nil {
			unitsInfoMap[unit.Filename] = info
		}
	}

	ext := filepath.Ext(unit.Filename)

	var warn error
	var convertErr error
	var service *parser.UnitFile
	switch ext {
	case ".container":
		service, warn, convertErr = quadlet.ConvertContainer(unit, unitsInfoMap, rootless)
	case ".network":
		service, warn, convertErr = quadlet.ConvertNetwork(unit, unitsInfoMap, rootless)
	case ".volume":
		service, warn, convertErr = quadlet.ConvertVolume(unit, unitsInfoMap, rootless)
	case ".kube":
		service, convertErr = quadlet.ConvertKube(unit, unitsInfoMap, rootless)
	default:
		return nil, fmt.Errorf("%s: unknown quadlet extension %q", unit.Filename, ext)
	}

	if warn != nil {
		slog.Warn("quadlet warning", "file", unit.Filename, "warning", warn)
	}
	if convertErr != nil {
		return nil, fmt.Errorf("%s: %w", unit.Filename, convertErr)
	}
	return service, nil
}
