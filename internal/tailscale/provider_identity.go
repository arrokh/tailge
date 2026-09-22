package tailscale

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/arrokh/tailge/internal/model"
)

// targetMatches retains the provider-local name while delegating target
// equivalence to the shared model policy.
func targetMatches(a, b model.Target) bool {
	return model.TargetsMatch(a, b)
}

func RoutesHash(routes []model.ExposureRoute) string {
	identities := make([]string, 0, len(routes))
	for _, route := range routes {
		identities = append(identities, IdentityOf(route).CanonicalKey())
	}
	return hashIDs(identities)
}

// RouteIDsHash fingerprints only the exact route IDs correlated with target.
// It is shared by exposure preconditions and the provider's final recheck.
func RouteIDsHash(routes []model.ExposureRoute, target model.Target) string {
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
