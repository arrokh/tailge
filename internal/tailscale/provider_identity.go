package tailscale

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/target"
)

// targetMatches retains the provider-local name while delegating target
// equivalence to the shared target policy.
func targetMatches(a, b target.Target) bool {
	return target.TargetsMatch(a, b)
}

func RoutesHash(routes []exposuredata.ExposureRoute) string {
	identities := make([]string, 0, len(routes))
	for _, route := range routes {
		identities = append(identities, IdentityOf(route).CanonicalKey())
	}
	return hashIDs(identities)
}

// RouteIDsHash fingerprints only the exact route IDs correlated with target.
// It is shared by exposure preconditions and the provider's final recheck.
func RouteIDsHash(routes []exposuredata.ExposureRoute, target target.Target) string {
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		if targetMatches(route.Target, target) {
			ids = append(ids, route.ID)
		}
	}
	return hashIDs(ids)
}

func hashIDs(ids []string) string {
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(h[:])
}
