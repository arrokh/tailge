package tailscale

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/arrokh/tailge/internal/model"
)

func targetMatches(a, b model.Target) bool {
	a, b = a.Normalized(), b.Normalized()
	if a.Protocol != b.Protocol || a.Port != b.Port {
		return false
	}
	if a.Address == b.Address {
		return true
	}
	if model.ScopeForAddress(a.Address) == model.ScopeLoopback && model.ScopeForAddress(b.Address) == model.ScopeLoopback {
		return true
	}
	return (model.ScopeForAddress(a.Address) == model.ScopeLoopback && (b.Address == "0.0.0.0" || b.Address == "::")) ||
		((a.Address == "0.0.0.0" || a.Address == "::") && (b.Address == "0.0.0.0" || b.Address == "::"))
}

func RoutesHash(routes []model.ExposureRoute) string {
	identities := make([]string, 0, len(routes))
	for _, route := range routes {
		identities = append(identities, IdentityOf(route).CanonicalKey())
	}
	return hashIDs(identities)
}

func hashIDs(ids []string) string {
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(h[:])
}
