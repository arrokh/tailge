package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/model"
)

func (a *Adapter) Status(ctx context.Context) (status Status, err error) {
	result, runErr := a.run(ctx, "status", "--json")
	if runErr != nil {
		return Status{}, a.commandError("status", result, runErr)
	}
	if result.Truncated {
		return Status{}, model.NewError(model.ErrUnknown, "tailscale", "status output was truncated", true, "partial", "Retry with a healthy Tailscale installation.")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return Status{}, model.NewError(model.ErrUnknown, "tailscale", "status emitted diagnostics: "+redact(strings.TrimSpace(result.Stderr)), true, "unknown", "Retry with a healthy Tailscale installation.")
	}
	if err := json.Unmarshal([]byte(result.Stdout), &status); err != nil {
		return Status{}, model.WrapError(model.ErrUnknown, "tailscale", "cannot parse status JSON", true, "unknown", "Upgrade or repair Tailscale, then retry.", err)
	}
	if err := validateStatusIPs(status); err != nil {
		return Status{}, model.WrapError(model.ErrUnknown, "tailscale", "status JSON contains an invalid node address", true, "unknown", "Retry after checking the Tailscale status output.", err)
	}
	return status, nil
}

type Status struct {
	BackendState string   `json:"BackendState"`
	AuthURL      string   `json:"AuthURL"`
	HaveNodeKey  bool     `json:"HaveNodeKey"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Self         struct {
		HostName     string   `json:"HostName"`
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
		Capabilities []string `json:"Capabilities"`
	} `json:"Self"`
}

func validateStatusIPs(status Status) error {
	for _, address := range append(append([]string{}, status.TailscaleIPs...), status.Self.TailscaleIPs...) {
		if net.ParseIP(strings.TrimSpace(address)) == nil {
			return fmt.Errorf("invalid node address %q", sanitizeText(address))
		}
	}
	return nil
}

func hasValidNodeAddress(status Status) bool {
	for _, address := range append(append([]string{}, status.TailscaleIPs...), status.Self.TailscaleIPs...) {
		if net.ParseIP(strings.TrimSpace(address)) != nil {
			return true
		}
	}
	return false
}

func (a *Adapter) List(ctx context.Context) (model.ExposureSnapshot, error) {
	now := a.now()
	snapshot := model.ExposureSnapshot{At: now, Source: "tailscale", Authoritative: false, Routes: []model.ExposureRoute{}}
	serve, serveErr := a.listMode(ctx, model.ExposureServe, now)
	funnel, funnelErr := a.listMode(ctx, model.ExposureFunnel, now)
	if serveErr != nil || funnelErr != nil {
		if serveErr != nil {
			snapshot.Warnings = append(snapshot.Warnings, serveErr.Error())
		}
		if funnelErr != nil {
			snapshot.Warnings = append(snapshot.Warnings, funnelErr.Error())
		}
		providerErr := aggregateReadError(serveErr, funnelErr)
		snapshot.Error = ptr(providerErr.Safe())
		snapshot.Routes = append(snapshot.Routes, serve.Routes...)
		snapshot.Routes = append(snapshot.Routes, funnel.Routes...)
		return snapshot, providerErr
	}
	snapshot.Routes = append(snapshot.Routes, serve.Routes...)
	snapshot.Routes = append(snapshot.Routes, funnel.Routes...)
	snapshot.Routes = dedupRoutes(snapshot.Routes)
	snapshot.Authoritative = true
	return snapshot, nil
}

func aggregateReadError(first, second error) *model.AppError {
	chosen := first
	if chosen == nil {
		chosen = second
	}
	if chosen == nil {
		return model.NewError(model.ErrUnknown, "tailscale", "exposure state is incomplete", true, "unknown", "Retry before changing any exposure.")
	}
	app := model.AsAppError(chosen)
	code := app.Code
	for _, candidate := range []error{first, second} {
		if candidate == nil {
			continue
		}
		next := model.AsAppError(candidate)
		switch next.Code {
		case model.ErrPermission:
			code = model.ErrPermission
		case model.ErrTimeout, model.ErrCancelled:
			if code != model.ErrPermission {
				code = next.Code
			}
		case model.ErrDependency:
			if code != model.ErrPermission && code != model.ErrTimeout && code != model.ErrCancelled {
				code = model.ErrDependency
			}
		case model.ErrUnknown:
			if code == model.ErrOperation {
				code = model.ErrUnknown
			}
		}
	}
	return model.WrapError(code, "tailscale", "exposure state is incomplete", true, "unknown", "Retry before changing any exposure.", chosen)
}

func (a *Adapter) listMode(ctx context.Context, mode model.ExposureMode, now time.Time) (model.ExposureSnapshot, error) {
	result, err := a.run(ctx, string(mode), "status", "--json")
	if err != nil {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, a.commandError(string(mode)+" status", result, err)
	}
	if result.Truncated {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status output was truncated", true, "partial", "Retry before changing exposure.")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status emitted diagnostics: "+redact(strings.TrimSpace(result.Stderr)), true, "unknown", "Retry with a healthy Tailscale installation.")
	}
	routes, err := parseStatus(mode, []byte(result.Stdout), now)
	if err != nil {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, err
	}
	return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode), Authoritative: true, Routes: routes}, nil
}

func parseStatus(mode model.ExposureMode, data []byte, now time.Time) ([]model.ExposureRoute, error) {
	var value any
	if strings.TrimSpace(string(data)) == "" {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status output was empty", true, "unknown", "Retry the Tailscale status command.")
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", "cannot parse "+string(mode)+" status JSON", true, "unknown", "Upgrade or repair Tailscale, then retry.", err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status JSON has an unsupported root shape", true, "unknown", "Retry after checking the Tailscale version.")
	}
	if len(root) > 0 && !recognizedStatusShape(value) {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status JSON has no recognized route fields", true, "unknown", "Retry after checking the Tailscale version.")
	}
	if err := validateStatusTargets(value, nil); err != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains an invalid target", true, "unknown", "Retry after checking the Tailscale version.", err)
	}
	if err := validateCompleteHandlers(value, nil); err != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains an unsupported or incomplete handler", true, "unknown", "Review the Tailscale configuration manually; tailge will not mutate incomplete state.", err)
	}
	permissions, permissionErr := funnelPermissions(value)
	if permissionErr != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains an invalid AllowFunnel field", true, "unknown", "Retry after checking the Tailscale status output.", permissionErr)
	}
	var routes []model.ExposureRoute
	walkStatus(value, nil, "", "", mode, now, &routes)
	routes = dedupRoutes(routes)
	if len(root) > 0 && len(routes) == 0 && !onlyFunnelPermissionStatus(value) {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains recognized fields but no complete routes", true, "unknown", "Retry after checking the Tailscale status output.")
	}
	routes = filterFunnelRoutes(routes, permissions)
	return routes, nil
}

const maxStatusDepth = 64

func validateCompleteHandlers(value any, path []string) error {
	if len(path) > maxStatusDepth {
		return fmt.Errorf("status JSON nesting exceeds %d levels", maxStatusDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := append(path, key)
			if strings.EqualFold(key, "handlers") {
				handlers, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("%s: handlers field must be an object", sanitizeText(strings.Join(childPath, "/")))
				}
				for handlerPath, rawHandler := range handlers {
					handler, ok := rawHandler.(map[string]any)
					if !ok {
						return fmt.Errorf("%s/%s: handler must be an object", sanitizeText(strings.Join(childPath, "/")), sanitizeText(handlerPath))
					}
					complete := false
					targetFields := 0
					for handlerKey, rawTarget := range handler {
						if !isTargetField(handlerKey) {
							continue
						}
						if target, ok := rawTarget.(string); ok && target != "" {
							targetFields++
							complete = true
						}
					}
					if !complete {
						return fmt.Errorf("%s/%s: handler has no supported proxy target", sanitizeText(strings.Join(childPath, "/")), sanitizeText(handlerPath))
					}
					if targetFields > 1 {
						return fmt.Errorf("%s/%s: handler has multiple proxy targets", sanitizeText(strings.Join(childPath, "/")), sanitizeText(handlerPath))
					}
				}
			}
			if err := validateCompleteHandlers(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := validateCompleteHandlers(child, append(path, strconv.Itoa(index))); err != nil {
				return err
			}
		}
	}
	return nil
}

func funnelPermissions(value any) (map[string]bool, error) {
	permissions := map[string]bool{}
	var walk func(any, int) error
	walk = func(current any, depth int) error {
		if depth > maxStatusDepth {
			return nil
		}
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if strings.EqualFold(key, "AllowFunnel") {
					if child == nil {
						continue
					}
					entries, ok := child.(map[string]any)
					if !ok {
						return fmt.Errorf("AllowFunnel must be an object")
					}
					for endpoint, raw := range entries {
						enabled, ok := raw.(bool)
						if !ok {
							return fmt.Errorf("AllowFunnel endpoint %q must be boolean", sanitizeText(endpoint))
						}
						if endpointPort(endpoint) == "" || portNumber(endpointPort(endpoint)) == 0 {
							return fmt.Errorf("AllowFunnel endpoint %q is not host:port", sanitizeText(endpoint))
						}
						permissions[endpoint] = enabled
					}
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(value, 0); err != nil {
		return nil, err
	}
	return permissions, nil
}

func filterFunnelRoutes(routes []model.ExposureRoute, permissions map[string]bool) []model.ExposureRoute {
	if len(permissions) == 0 {
		if len(routes) == 0 {
			return routes
		}
		filtered := make([]model.ExposureRoute, 0, len(routes))
		for _, route := range routes {
			if route.Mode == model.ExposureServe {
				filtered = append(filtered, route)
			}
		}
		return filtered
	}
	filtered := make([]model.ExposureRoute, 0, len(routes))
	for _, route := range routes {
		funnel := funnelEnabledForRoute(route, permissions)
		if (route.Mode == model.ExposureFunnel && funnel) || (route.Mode == model.ExposureServe && !funnel) {
			filtered = append(filtered, route)
		}
	}
	return filtered
}

func funnelEnabledForRoute(route model.ExposureRoute, permissions map[string]bool) bool {
	ports := map[string]bool{strconv.Itoa(route.Target.Port): true}
	if _, selector, ok := strings.Cut(route.ProviderKey, ":"); ok {
		if _, providerPort, ok := strings.Cut(selector, "="); ok && providerPort != "" {
			ports[providerPort] = true
		}
	}
	endpoint := ""
	if route.URL != "" {
		if parsed, err := url.Parse(route.URL); err == nil && parsed.Host != "" {
			endpoint = normalizeEndpoint(parsed.Host)
		}
	}
	for rawEndpoint, enabled := range permissions {
		if !enabled {
			continue
		}
		if endpoint != "" {
			if endpoint == normalizeEndpoint(rawEndpoint) {
				return true
			}
			continue
		}
		if ports[endpointPort(rawEndpoint)] {
			return true
		}
	}
	return false
}

func normalizeEndpoint(value string) string {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return strings.ToLower(net.JoinHostPort(strings.TrimSuffix(host, "."), port))
}

func endpointPort(value string) string {
	if _, port, err := net.SplitHostPort(value); err == nil {
		return port
	}
	if index := strings.LastIndexByte(value, ':'); index >= 0 && index+1 < len(value) {
		return value[index+1:]
	}
	return ""
}

func onlyFunnelPermissionStatus(value any) bool {
	root, ok := value.(map[string]any)
	if !ok || len(root) == 0 {
		return false
	}
	for key := range root {
		if !strings.EqualFold(key, "AllowFunnel") {
			return false
		}
	}
	return true
}

func recognizedStatusShape(value any) bool {
	recognized := false
	var walk func(any, int)
	walk = func(current any, depth int) {
		if recognized || depth > maxStatusDepth {
			return
		}
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				lower := strings.ToLower(key)
				if lower == "web" || lower == "tcp" || lower == "https" || lower == "handlers" || lower == "proxy" || lower == "target" || lower == "backend" || lower == "handler" || lower == "tcpforward" || lower == "service" || lower == "servicename" || lower == "allowfunnel" || strings.HasPrefix(lower, "svc:") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") || publicPort(key) > 0 {
					recognized = true
					return
				}
				walk(child, depth+1)
			}
		case []any:
			for _, child := range typed {
				walk(child, depth+1)
			}
		}
	}
	walk(value, 0)
	return recognized
}

func validateStatusTargets(value any, path []string) error {
	if len(path) > maxStatusDepth {
		return fmt.Errorf("status JSON nesting exceeds %d levels", maxStatusDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := append(path, key)
			lowerKey := strings.ToLower(key)
			if lowerKey == "tcp" || lowerKey == "web" || lowerKey == "handlers" {
				container, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("%s: %s field must be an object", sanitizeText(strings.Join(childPath, "/")), key)
				}
				if lowerKey == "tcp" {
					for port, route := range container {
						switch typedRoute := route.(type) {
						case string:
							if _, err := model.ParseTarget(typedRoute, "tcp"); err != nil {
								return fmt.Errorf("%s/%s: invalid TCP target: %w", sanitizeText(strings.Join(childPath, "/")), sanitizeText(port), err)
							}
						case map[string]any:
							// Tailscale may use an object containing transport metadata
							// (for example HTTPS: true) alongside target fields.
						default:
							return fmt.Errorf("%s/%s: TCP route must be an object or target string", sanitizeText(strings.Join(childPath, "/")), sanitizeText(port))
						}
					}
				}
				if lowerKey == "web" {
					for host, route := range container {
						if _, ok := route.(map[string]any); !ok {
							return fmt.Errorf("%s/%s: Web route must be an object", sanitizeText(strings.Join(childPath, "/")), sanitizeText(host))
						}
					}
				}
			}
			if lowerKey == "service" || lowerKey == "servicename" {
				if _, ok := child.(string); !ok {
					return fmt.Errorf("%s: service field must be a string", sanitizeText(strings.Join(childPath, "/")))
				}
			}
			if isTargetField(key) || strings.EqualFold(key, "tcpforward") {
				targetText, ok := child.(string)
				if !ok {
					return fmt.Errorf("%s: target field must be a string", sanitizeText(strings.Join(childPath, "/")))
				}
				if _, err := model.ParseTarget(targetText, "tcp"); err != nil {
					return fmt.Errorf("%s: %w", sanitizeText(strings.Join(childPath, "/")), err)
				}
			}
			if err := validateStatusTargets(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range typed {
			if err := validateStatusTargets(child, append(path, strconv.Itoa(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkStatus(value any, path []string, urlHint, serviceHint string, mode model.ExposureMode, now time.Time, routes *[]model.ExposureRoute) {
	if len(path) > maxStatusDepth {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		mapServiceHint := serviceHint
		for key, child := range typed {
			keys = append(keys, key)
			if strings.EqualFold(key, "Service") || strings.EqualFold(key, "ServiceName") {
				if mapService, ok := child.(string); ok {
					mapServiceHint = mapService
				}
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := typed[key]
			newPath := append(append([]string{}, path...), key)
			nextURL, nextService := urlHint, mapServiceHint
			lowerKey := strings.ToLower(key)
			if strings.HasPrefix(lowerKey, "https://") || strings.HasPrefix(lowerKey, "http://") {
				nextURL = redact(key)
			}
			if !strings.Contains(key, "://") {
				if port := publicPort(key); port > 0 {
					nextURL = "https://" + key
				}
			}
			if strings.HasPrefix(strings.ToLower(key), "svc:") {
				nextService = key
			}
			if strings.EqualFold(key, "Service") || strings.EqualFold(key, "ServiceName") {
				if s, ok := child.(string); ok {
					nextService = s
				}
			}
			if isTargetField(key) {
				if targetString, ok := child.(string); ok {
					if target, err := model.ParseTarget(targetString, "tcp"); err == nil {
						transport := ""
						if containsPath(newPath, "web") {
							transport = "https"
						} else if containsPath(newPath, "tcp") {
							transport = "tcp"
						}
						providerKey := routeSelectorForPort(mode, transport, pathPort(newPath))
						// A service name is not itself a listener selector. Without a
						// public port from the status path, keep ProviderKey empty so
						// removal and rollback fail closed instead of guessing.
						pathValue := handlerPath(newPath)
						id := routeID(mode, target, nextURL, nextService, pathValue, targetString, strings.Join(newPath, "/"))
						*routes = append(*routes, model.ExposureRoute{ID: id, ProviderKey: providerKey, Service: nextService, Path: pathValue, Backend: targetString, Target: target, Mode: mode, URL: redact(nextURL), Ownership: model.OwnershipUnknown, State: model.ExposureActive, LastSeen: now, Source: "tailscale " + string(mode)})
					}
				}
			}
			if strings.EqualFold(key, "TCP") {
				walkTCP(child, newPath, nextURL, nextService, mode, now, routes)
				continue
			}
			walkStatus(child, newPath, nextURL, nextService, mode, now, routes)
		}
	case []any:
		for index, child := range typed {
			walkStatus(child, append(path, strconv.Itoa(index)), urlHint, serviceHint, mode, now, routes)
		}
	}
}

func walkTCP(value any, path []string, urlHint, serviceHint string, mode model.ExposureMode, now time.Time, routes *[]model.ExposureRoute) {
	objects, ok := value.(map[string]any)
	if !ok {
		return
	}
	for portText, child := range objects {
		var targetText, transport string
		switch v := child.(type) {
		case string:
			targetText, transport = v, "tcp"
		case map[string]any:
			for _, wantedKey := range []string{"TCPForward", "Proxy", "Target", "Backend", "Handler", "HTTP", "HTTPS"} {
				for actualKey, rawTarget := range v {
					if !strings.EqualFold(actualKey, wantedKey) {
						continue
					}
					if s, ok := rawTarget.(string); ok {
						targetText = s
						// The enclosing TCP container is authoritative for the
						// public transport. Do not infer HTTPS from a nested
						// target-field name such as Proxy.
						transport = "tcp"
					}
					break
				}
				if targetText != "" {
					break
				}
			}
		}
		if targetText == "" {
			continue
		}
		if target, err := model.ParseTarget(targetText, "tcp"); err == nil {
			if target.Port == 0 {
				if p, e := strconv.Atoi(portText); e == nil {
					target.Port = p
				}
			}
			providerKey := routeSelectorForPort(mode, transport, portNumber(portText))
			// The TCP map key is the exact public listener selector; do not
			// replace it with a service name when it is unavailable.
			id := routeID(mode, target, urlHint, serviceHint, "", targetText, strings.Join(path, "/")+"/"+portText)
			*routes = append(*routes, model.ExposureRoute{ID: id, ProviderKey: providerKey, Service: serviceHint, Backend: targetText, Target: target, Mode: mode, URL: redact(urlHint), Ownership: model.OwnershipUnknown, State: model.ExposureActive, LastSeen: now, Source: "tailscale " + string(mode)})
		}
	}
}

func publicPort(value string) int {
	colon := strings.LastIndexByte(value, ':')
	if colon < 1 || colon == len(value)-1 {
		return 0
	}
	return portNumber(value[colon+1:])
}

func containsPath(path []string, wanted string) bool {
	for _, part := range path {
		if strings.EqualFold(part, wanted) {
			return true
		}
	}
	return false
}

func pathPort(path []string) int {
	for i := len(path) - 1; i >= 0; i-- {
		if port := publicPort(path[i]); port > 0 {
			return port
		}
	}
	return 0
}
