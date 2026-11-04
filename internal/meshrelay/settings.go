package meshrelay

import (
	"strings"

	"github.com/dpup/prefab/errors"

	"github.com/dpup/sierra-data/internal/config"
)

// ConfigFrom maps prefab.yaml's meshRelay section onto the relay's runtime
// Config. A malformed admin entry is an error, not a skip: a skipped admin
// would silently never authenticate, which is the push-ingest reporter rule.
func ConfigFrom(c config.MeshRelayConfig) (Config, error) {
	admins := map[string]string{}
	for i, a := range c.Admins {
		hash := strings.ToLower(strings.TrimSpace(a.TokenSHA256))
		switch {
		case !ValidClientName(a.Name):
			return Config{}, errors.Errorf("meshRelay.admins[%d]: name %q must be a lowercase slug", i, a.Name)
		case len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "":
			return Config{}, errors.Errorf("meshRelay.admins[%d] (%s): tokenSha256 must be 64 hex characters "+
				"(the token's hash, never the token — mint with make relay-token)", i, a.Name)
		}
		if _, dup := admins[a.Name]; dup {
			return Config{}, errors.Errorf("meshRelay.admins: %q is listed twice", a.Name)
		}
		admins[a.Name] = hash
	}
	return Config{
		LeaseDuration: c.LeaseDuration, LeaseIdleTimeout: c.LeaseIdleTimeout, LeaseWarning: c.LeaseWarning,
		MaxLeaseTotal: c.MaxLeaseTotal, HelloTimeout: c.HelloTimeout, PingInterval: c.PingInterval,
		FrameRate: c.FrameRate, FrameBurst: c.FrameBurst,
		AuthFailLimit: c.AuthFailLimit, AuthFailWindow: c.AuthFailWindow, AuthLockout: c.AuthLockout,
		ClientIPHeader: c.ClientIPHeader, BootstrapAdmins: admins, DebugPayloadGateways: c.DebugPayloadGateways,
	}, nil
}
