package server_test

import (
	"os"
	"testing"

	"freebuff-proxy/internal/testutil"
)

// TestMain strips ambient freebuff-proxy config env vars so a developer's
// exported proxy environment cannot leak into config.Load with higher
// precedence than .env and break mode-switch / token-persistence assertions.
func TestMain(m *testing.M) {
	testutil.UnsetConfigEnvForTestMain()
	os.Exit(m.Run())
}
