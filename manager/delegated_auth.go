package manager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	delegationRefreshBuffer = time.Minute
	delegationHTTPTimeout   = 10 * time.Second
	delegationResponseLimit = 1 << 20
	firstPartyChatClientID  = "tinfoil-chat"
	chatProduct             = "chat"
)

const delegationSecretHeader = "X-Tinfoil-Delegation-Secret"

type AuthorizationProvider interface {
	Authorization(context.Context) (string, error)
}

type authorizationProviderKey struct{}

func WithAuthorizationProvider(ctx context.Context, provider AuthorizationProvider) context.Context {
	return context.WithValue(ctx, authorizationProviderKey{}, provider)
}

func authorizationFromContext(ctx context.Context) (string, bool, error) {
	provider, ok := ctx.Value(authorizationProviderKey{}).(AuthorizationProvider)
	if !ok {
		return "", false, nil
	}
	authorization, err := provider.Authorization(ctx)
	if err != nil {
		return "", true, err
	}
	return authorization, true, nil
}

func AuthorizationProviderFromContext(ctx context.Context) AuthorizationProvider {
	provider, _ := ctx.Value(authorizationProviderKey{}).(AuthorizationProvider)
	return provider
}

func IsFirstPartyChatAccessJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var metadata struct {
		Type string `json:"typ"`
	}
	if err := json.Unmarshal(header, &metadata); err != nil {
		return false
	}
	if strings.TrimPrefix(strings.ToLower(metadata.Type), "application/") != "at+jwt" {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		ClientID string `json:"client_id"`
		Product  string `json:"product"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false
	}
	return claims.ClientID == firstPartyChatClientID && claims.Product == chatProduct
}

type delegationRequest struct {
	SubjectToken  string `json:"subject_token,omitempty"`
	RootRequestID string `json:"root_request_id,omitempty"`
	GrantToken    string `json:"grant_token,omitempty"`
}

type delegationResponse struct {
	AccessToken          string    `json:"access_token"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	GrantToken           string    `json:"grant_token"`
	GrantExpiresAt       time.Time `json:"grant_expires_at"`
}

type delegatedAuthorization struct {
	mu                   sync.Mutex
	refreshing           *delegationRefresh
	client               *http.Client
	endpoint             string
	secret               string
	now                  func() time.Time
	tokens               delegationResponse
	useAccessUntilExpiry bool
}

type delegationRefresh struct {
	done chan struct{}
	err  error
}

type DelegationHTTPError struct {
	StatusCode int
	RetryAfter string
}

func (e *DelegationHTTPError) Error() string {
	return fmt.Sprintf("delegation endpoint returned status %d", e.StatusCode)
}

func validateDelegationControlPlaneURL(controlPlaneURL string, debug bool) error {
	if debug {
		return nil
	}
	parsed, err := url.Parse(controlPlaneURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("control plane URL must use HTTPS for inference delegation")
	}
	return nil
}

func (em *EnclaveManager) NewDelegatedAuthorizationProvider(ctx context.Context, subjectToken, rootRequestID string) (AuthorizationProvider, error) {
	provider := &delegatedAuthorization{
		client:   em.delegationHTTPClient,
		endpoint: strings.TrimRight(em.controlPlaneURL, "/") + "/api/internal/inference/delegate",
		secret:   em.inferenceDelegationSecret,
		now:      time.Now,
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, delegationHTTPTimeout)
	defer cancel()
	tokens, err := provider.exchange(exchangeCtx, delegationRequest{SubjectToken: subjectToken, RootRequestID: rootRequestID})
	if err != nil {
		return nil, fmt.Errorf("exchange inference credential: %w", err)
	}
	provider.tokens = tokens
	provider.useAccessUntilExpiry = !provider.now().Add(delegationRefreshBuffer).Before(tokens.AccessTokenExpiresAt)
	return provider, nil
}

func (em *EnclaveManager) WithDelegatedAuthorization(ctx context.Context, authorization, rootRequestID string) (context.Context, bool, error) {
	subjectToken := BearerToken(authorization)
	if !IsFirstPartyChatAccessJWT(subjectToken) {
		return ctx, false, nil
	}
	provider, err := em.NewDelegatedAuthorizationProvider(ctx, subjectToken, rootRequestID)
	if err != nil {
		return ctx, true, err
	}
	return WithAuthorizationProvider(ctx, provider), true, nil
}

func (p *delegatedAuthorization) Authorization(ctx context.Context) (string, error) {
	for {
		p.mu.Lock()
		now := p.now()
		if now.Before(p.tokens.AccessTokenExpiresAt) && (p.useAccessUntilExpiry || now.Add(delegationRefreshBuffer).Before(p.tokens.AccessTokenExpiresAt)) {
			token := p.tokens.AccessToken
			p.mu.Unlock()
			return "Bearer " + token, nil
		}
		if refresh := p.refreshing; refresh != nil {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-refresh.done:
				if refresh.err != nil {
					return "", fmt.Errorf("refresh inference credential: %w", refresh.err)
				}
				continue
			}
		}
		refresh := &delegationRefresh{done: make(chan struct{})}
		p.refreshing = refresh
		grantToken := p.tokens.GrantToken
		grantExpiresAt := p.tokens.GrantExpiresAt
		p.mu.Unlock()
		go p.refresh(refresh, grantToken, grantExpiresAt)

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-refresh.done:
			if refresh.err != nil {
				return "", fmt.Errorf("refresh inference credential: %w", refresh.err)
			}
		}
	}
}

func (p *delegatedAuthorization) refresh(refresh *delegationRefresh, grantToken string, grantExpiresAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), delegationHTTPTimeout)
	defer cancel()

	var tokens delegationResponse
	var err error
	if grantToken == "" || !p.now().Before(grantExpiresAt) {
		err = fmt.Errorf("delegation grant expired")
	} else {
		tokens, err = p.exchange(ctx, delegationRequest{GrantToken: grantToken})
	}

	p.mu.Lock()
	if err == nil {
		p.tokens = tokens
		p.useAccessUntilExpiry = !p.now().Add(delegationRefreshBuffer).Before(tokens.AccessTokenExpiresAt)
	}
	refresh.err = err
	p.refreshing = nil
	close(refresh.done)
	p.mu.Unlock()
}

func (p *delegatedAuthorization) exchange(ctx context.Context, payload delegationRequest) (delegationResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return delegationResponse{}, fmt.Errorf("encode delegation request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return delegationResponse{}, fmt.Errorf("build delegation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(delegationSecretHeader, p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		return delegationResponse{}, fmt.Errorf("send delegation request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return delegationResponse{}, &DelegationHTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: resp.Header.Get("Retry-After"),
		}
	}
	var result delegationResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, delegationResponseLimit))
	if err := decoder.Decode(&result); err != nil {
		return delegationResponse{}, fmt.Errorf("decode delegation response: %w", err)
	}
	if result.AccessToken == "" || result.GrantToken == "" || result.AccessTokenExpiresAt.IsZero() || result.GrantExpiresAt.IsZero() {
		return delegationResponse{}, fmt.Errorf("delegation endpoint returned incomplete credentials")
	}
	if now := p.now(); !now.Before(result.AccessTokenExpiresAt) || !now.Before(result.GrantExpiresAt) {
		return delegationResponse{}, fmt.Errorf("delegation endpoint returned expired credentials")
	}
	return result, nil
}
