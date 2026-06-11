package agentcfg

import (
	"github.com/moby/sys/mountinfo"
)

var detectHostDataDirFunc = detectHostDataDir

// detectHostDataDir returns the host-side source path of the bind mount at
// dataDir when picolet runs containerized, or "" when dataDir is not a mount
// point (native runs) or the source path cannot be trusted.
//
// The mountinfo root field is relative to the root of the mount's SOURCE
// filesystem, not the host path namespace. On a single-partition host (stock
// Raspberry Pi OS) both coincide; when the bind source lives on a separate
// partition or a btrfs subvolume the field is not a usable host path — btrfs
// is rejected here, other layouts need an explicit host_data_dir override.
func detectHostDataDir(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	entries, err := mountinfo.GetMounts(mountinfo.SingleEntryFilter(dataDir))
	if err != nil || len(entries) == 0 {
		return ""
	}
	entry := entries[0]
	if entry.FSType == "btrfs" {
		return "" // Root carries a subvolume prefix, not a host path
	}
	return entry.Root
}
