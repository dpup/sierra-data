package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
)

// TestE2EAgainstRelay runs the tool's full e2e scenario against an in-process
// relay on a real prefab server, so the tool and the relay cannot drift apart.
func TestE2EAgainstRelay(t *testing.T) {
	config.RegisterPrefabNamespaces()
	st, err := meshrelay.OpenStore(context.Background(), filepath.Join(t.TempDir(), "relay.db"), time.Second)
	require.NoError(t, err)
	defer st.Close()
	admin, err := meshrelay.MintToken(meshrelay.ClientTokenPrefix)
	require.NoError(t, err)
	relay := meshrelay.New(context.Background(), meshrelay.Config{
		BootstrapAdmins: map[string]string{"e2e": meshrelay.TokenHash(admin)},
	}, st)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	server := prefab.New(prefab.WithHost("127.0.0.1"), prefab.WithPort(port), prefab.WithPlugin(relay))
	go func() { _ = server.Start() }()
	defer func() { _ = server.Shutdown() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + prefix + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)

	require.Equal(t, 0, runE2E(base, admin, false), "e2e scenario failed (see output)")
}
