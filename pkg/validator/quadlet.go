package validator

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/config"
)

// quadletConverter wires a CheckQuadlet category row to Podman's converter.
type quadletConverter struct {
	// convert returns the generated service, a non-fatal warning, and an error.
	convert func(unit *parser.UnitFile, units map[string]*quadlet.UnitInfo, rootless bool) (*parser.UnitFile, error, error)
	// resourceName prefills UnitInfo.ResourceName; set iff the row has Prefill.
	resourceName func(unit *parser.UnitFile) string
}

// quadletConverters holds the converter for every CheckQuadlet category.
var quadletConverters = map[config.Category]quadletConverter{
	config.CategoryNetwork: {convert: quadlet.ConvertNetwork},
	config.CategoryVolume:  {convert: quadlet.ConvertVolume},
	config.CategoryContainer: {
		convert:      quadlet.ConvertContainer,
		resourceName: quadlet.GetContainerResourceName,
	},
	config.CategoryKube: {convert: withoutWarning(quadlet.ConvertKube)},
	config.CategoryPod: {
		convert:      quadlet.ConvertPod,
		resourceName: quadlet.GetPodResourceName,
	},
	config.CategoryImage: {convert: withoutWarning(quadlet.ConvertImage)},
	config.CategoryBuild: {
		convert:      quadlet.ConvertBuild,
		resourceName: quadlet.GetBuiltImageName,
	},
}

func withoutWarning(convert func(*parser.UnitFile, map[string]*quadlet.UnitInfo, bool) (*parser.UnitFile, error)) func(*parser.UnitFile, map[string]*quadlet.UnitInfo, bool) (*parser.UnitFile, error, error) {
	return func(unit *parser.UnitFile, units map[string]*quadlet.UnitInfo, rootless bool) (*parser.UnitFile, error, error) {
		service, err := convert(unit, units, rootless)
		return service, nil, err
	}
}

// buildUnitInfo mirrors Podman's generateUnitsInfoMap logic exactly.
// GetUnitServiceName returns the base service name without ".service" suffix;
// quadlet.UnitInfo.ServiceFileName appends the suffix when needed.
// ResourceName is pre-filled for categories whose row has Prefill (e.g.
// .container, for network reuse resolution via GetContainerResourceName);
// Convert* fills it for the others.
func buildUnitInfo(unit *parser.UnitFile) *quadlet.UnitInfo {
	serviceName, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		return nil
	}
	info := &quadlet.UnitInfo{ServiceName: serviceName}
	if category, ok := config.CategoryForExtension(filepath.Ext(unit.Filename)); ok {
		if conv := quadletConverters[category]; conv.resourceName != nil {
			info.ResourceName = conv.resourceName(unit)
		}
	}
	return info
}

// validateQuadlet validates a pre-parsed quadlet unit against the units info map.
// The unit must already be parsed by the caller (via ValidateFiles or validateFile).
// Podman's Convert* functions require the unit's own entry in unitsInfoMap
// (via initServiceUnitFile), so we ensure it is populated before converting.
// rootless must be true when validating units for rootless Podman; it controls
// systemd dependency generation (e.g. podman-user-wait-network-online.service vs
// network-online.target).
func validateQuadlet(unit *parser.UnitFile, unitsInfoMap map[string]*quadlet.UnitInfo, rootless bool) error {
	_, err := convertQuadlet(unit, unitsInfoMap, rootless)
	return err
}

func convertQuadlet(unit *parser.UnitFile, unitsInfoMap map[string]*quadlet.UnitInfo, rootless bool) (*parser.UnitFile, error) {
	// Ensure the unit's own entry is in the map. In the ValidateFiles path this is
	// pre-populated by buildUnitsInfoFromFiles; this handles direct calls (e.g. tests).
	if _, ok := unitsInfoMap[unit.Filename]; !ok {
		if info := buildUnitInfo(unit); info != nil {
			unitsInfoMap[unit.Filename] = info
		}
	}

	ext := filepath.Ext(unit.Filename)
	category, _ := config.CategoryForExtension(ext)
	spec, ok := config.SpecFor(category)
	if !ok || spec.Dest != config.DestQuadlet {
		return nil, fmt.Errorf("%s: unknown quadlet extension %q", unit.Filename, ext)
	}
	conv, ok := quadletConverters[category]
	if !ok {
		return nil, unsupportedError(unit.Filename, ext)
	}

	service, warn, convertErr := conv.convert(unit, unitsInfoMap, rootless)
	if warn != nil {
		slog.Warn("quadlet warning", "file", unit.Filename, "warning", warn)
	}
	if convertErr != nil {
		return nil, fmt.Errorf("%s: %w", unit.Filename, convertErr)
	}
	return service, nil
}

// rejectSelfPodMember rejects the agent's own container joining a pod. The
// member is BindsTo= the pod's service, so restarting the pod on a change, or
// stopping it when the pod file is deleted, would stop the agent before it
// saves state.
func rejectSelfPodMember(unit *parser.UnitFile) error {
	pod, _ := unit.Lookup(quadlet.ContainerGroup, quadlet.KeyPod)
	if pod == "" {
		return nil
	}
	name, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		return nil //nolint:nilerr // conversion already rejected a unit Podman cannot name
	}
	if service := name + ".service"; config.IsDefaultSelfUnit(service) {
		return fmt.Errorf("%s: the agent's own unit %s must not join a pod (Pod=%s): "+
			"restarting or stopping the pod would stop the agent before it saves state", unit.Filename, service, pod)
	}
	return nil
}

func unsupportedError(path, ext string) error {
	return fmt.Errorf("%s: `%s` is not supported by picolet", path, ext)
}
