package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

const (
	routeContextPath          = "/api/shim/route-context"
	routeContextLookupTimeout = 2 * time.Second
	routeContextCacheTTL      = time.Hour
)

type routeContextClient struct {
	endpoint   string
	httpClient *http.Client
	ttl        time.Duration
	mu         sync.RWMutex
	cache      map[string]cachedRouteContext
}

type cachedRouteContext struct {
	context routeContext
	expires time.Time
}

type routeContextRequest struct {
	APIKey string `json:"api_key"`
}

type routeContext struct {
	UserID   string `json:"user_id"`
	OrgID    string `json:"org_id,omitempty"`
	Priority *int   `json:"priority,omitempty"`
}

func newRouteContextClient(controlPlaneURL string) *routeContextClient {
	return &routeContextClient{
		endpoint:   strings.TrimRight(controlPlaneURL, "/") + routeContextPath,
		httpClient: http.DefaultClient,
		ttl:        routeContextCacheTTL,
		cache:      make(map[string]cachedRouteContext),
	}
}

func (c *routeContextClient) Lookup(ctx context.Context, apiKey, model string) (routeContext, bool) {
	if c == nil || c.endpoint == "" || apiKey == "" || model == "" {
		return routeContext{}, false
	}

	key := routeContextCacheKey(apiKey)
	now := time.Now()
	c.mu.RLock()
	cached, ok := c.cache[key]
	c.mu.RUnlock()
	if ok && now.Before(cached.expires) {
		return cached.context, true
	}

	resolved, ok := c.fetch(ctx, apiKey, model)
	if !ok {
		return routeContext{}, false
	}

	c.mu.Lock()
	c.cache[key] = cachedRouteContext{
		context: resolved,
		expires: now.Add(c.ttl),
	}
	c.mu.Unlock()

	return resolved, true
}

func (c *routeContextClient) fetch(ctx context.Context, apiKey, model string) (routeContext, bool) {
	body, err := json.Marshal(routeContextRequest{APIKey: apiKey})
	if err != nil {
		manager.RouteContextLookupFailuresTotal.WithLabelValues(model, "marshal").Inc()
		return routeContext{}, false
	}

	reqCtx, cancel := context.WithTimeout(ctx, routeContextLookupTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		manager.RouteContextLookupFailuresTotal.WithLabelValues(model, "request").Inc()
		return routeContext{}, false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		manager.RouteContextLookupFailuresTotal.WithLabelValues(model, "transport").Inc()
		return routeContext{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		manager.RouteContextLookupFailuresTotal.WithLabelValues(model, fmt.Sprintf("http_%d", resp.StatusCode)).Inc()
		return routeContext{}, false
	}

	var resolved routeContext
	if err := json.NewDecoder(resp.Body).Decode(&resolved); err != nil {
		manager.RouteContextLookupFailuresTotal.WithLabelValues(model, "decode").Inc()
		return routeContext{}, false
	}

	return resolved, true
}

func routeContextCacheKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}
