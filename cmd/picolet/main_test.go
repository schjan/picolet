package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
)

func TestDataDirFromConfigUsesConfiguredDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "picolet-data")
	cfg := &agentcfg.Config{DataDir: dataDir, Rootless: true}

	got, err := dataDirFromConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, dataDir, got)

	lockPath, err := lockPathFromConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dataDir, "reconciliation.lock"), lockPath)
}

func TestDataDirFromConfigUsesRootfulDefault(t *testing.T) {
	cfg := &agentcfg.Config{}

	got, err := dataDirFromConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/picolet", got)

	lockPath, err := lockPathFromConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/picolet/reconciliation.lock", lockPath)
}

func TestDataDirFromConfigUsesRootlessDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &agentcfg.Config{Rootless: true}

	got, err := dataDirFromConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".local", "share", "picolet"), got)

	lockPath, err := lockPathFromConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".local", "share", "picolet", "reconciliation.lock"), lockPath)
}
