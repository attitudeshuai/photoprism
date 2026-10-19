package photoprism

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/config"
	"github.com/photoprism/photoprism/pkg/fs"
)

// newThumbsStatusTestConfig builds an isolated config with a database and storage directories,
// and installs it as the package config so MediaFile root detection works.
func newThumbsStatusTestConfig(t *testing.T, dbName string) *config.Config {
	t.Helper()

	cfg := config.NewMinimalTestConfigWithDb(dbName, t.TempDir())
	require.NoError(t, cfg.CreateDirectories())

	oldCfg := Config()
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(oldCfg) })

	return cfg
}

// copyFixtureToOriginals copies a package test fixture into the configured originals folder
// and returns the absolute destination path.
func copyFixtureToOriginals(t *testing.T, cfg *config.Config, src, destName string) string {
	t.Helper()

	data, err := os.ReadFile(src)
	require.NoError(t, err)

	dest := filepath.Join(cfg.OriginalsPath(), filepath.FromSlash(destName))
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), fs.ModeDir))
	require.NoError(t, os.WriteFile(dest, data, fs.ModeFile))

	t.Cleanup(func() { _ = os.Remove(dest) })

	return dest
}
