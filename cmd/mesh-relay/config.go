package main

import (
	"encoding/base64"
	"strings"

	"github.com/dpup/prefab/errors"

	"github.com/sierragridteam/sierra-data/internal/config"

	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
)

// relayConfigFrom maps prefab.yaml's meshRelay section onto the relay's
// runtime Config. A malformed admin entry is an error, not a skip: a skipped
// admin would silently never authenticate, which is the push-ingest reporter
// rule. The handoff gate is added at activation.
func relayConfigFrom(c config.MeshRelayConfig) (meshrelay.Config, error) {
	admins := map[string]string{}
	for i, a := range c.Admins {
		hash := strings.ToLower(strings.TrimSpace(a.TokenSHA256))
		switch {
		case !meshrelay.ValidClientName(a.Name):
			return meshrelay.Config{}, errors.Errorf("meshRelay.admins[%d]: name %q must be a lowercase slug", i, a.Name)
		case len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "":
			return meshrelay.Config{}, errors.Errorf("meshRelay.admins[%d] (%s): tokenSha256 must be 64 hex characters "+
				"(the token's hash, never the token — mint with make relay-token)", i, a.Name)
		}
		if _, dup := admins[a.Name]; dup {
			return meshrelay.Config{}, errors.Errorf("meshRelay.admins: %q is listed twice", a.Name)
		}
		admins[a.Name] = hash
	}
	var secretKey []byte
	if k := strings.TrimSpace(c.SecretKey); k != "" {
		b, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(b) != 32 {
			return meshrelay.Config{}, errors.Errorf("meshRelay.secretKey (PF__MESH_RELAY__SECRET_KEY) must be 32 bytes, " +
				"base64 (openssl rand -base64 32)")
		}
		secretKey = b
	}
	return meshrelay.Config{
		SecretKey:     secretKey,
		LeaseDuration: c.LeaseDuration, LeaseIdleTimeout: c.LeaseIdleTimeout, LeaseWarning: c.LeaseWarning,
		MaxLeaseTotal: c.MaxLeaseTotal, HelloTimeout: c.HelloTimeout, PingInterval: c.PingInterval,
		FrameRate: c.FrameRate, FrameBurst: c.FrameBurst,
		AuthFailLimit: c.AuthFailLimit, AuthFailWindow: c.AuthFailWindow, AuthLockout: c.AuthLockout,
		ClientIPHeader: c.ClientIPHeader, BootstrapAdmins: admins, DebugPayloadGateways: c.DebugPayloadGateways,
	}, nil
}
