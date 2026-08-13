package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func accessJWT(t *testing.T, clientID, product string) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "at+jwt"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"client_id": clientID, "product": product})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func typedAccessJWT(t *testing.T) string {
	t.Helper()
	return accessJWT(t, firstPartyChatClientID, chatProduct)
}

func writeDelegationResponse(t *testing.T, w http.ResponseWriter, accessToken, grantToken string, accessExpiry, grantExpiry time.Time) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(delegationResponse{
		AccessToken:          accessToken,
		AccessTokenExpiresAt: accessExpiry,
		GrantToken:           grantToken,
		GrantExpiresAt:       grantExpiry,
	}); err != nil {
		t.Errorf("encode delegation response: %v", err)
	}
}

func TestIsFirstPartyChatAccessJWT(t *testing.T) {
	if !IsFirstPartyChatAccessJWT(typedAccessJWT(t)) {
		t.Fatal("expected first-party Chat at+jwt token to be recognized")
	}
	for _, token := range []string{
		"opaque-key",
		"e30.e30.signature",
		"bad.payload.signature",
		accessJWT(t, "third-party", chatProduct),
		accessJWT(t, firstPartyChatClientID, "api"),
	} {
		if IsFirstPartyChatAccessJWT(token) {
			t.Fatalf("ineligible credential %q must not be delegated", token)
		}
	}
}

func TestIneligibleAuthorizationDoesNotExchange(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	em := &EnclaveManager{controlPlaneURL: server.URL, delegationHTTPClient: server.Client()}
	ctx := context.Background()

	for _, authorization := range []string{
		"Bearer opaque-key",
		"Bearer " + accessJWT(t, "third-party", chatProduct),
		"Bearer " + accessJWT(t, firstPartyChatClientID, "api"),
	} {
		gotCtx, delegated, err := em.WithDelegatedAuthorization(ctx, authorization, "root")
		if err != nil {
			t.Fatal(err)
		}
		if delegated || gotCtx != ctx {
			t.Fatalf("ineligible authorization must preserve the original context: %q", authorization)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("delegation calls = %d, want 0", got)
	}
}

func TestDelegatedAuthorizationInitialExchangeAndRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	subjectToken := typedAccessJWT(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/internal/inference/delegate" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get(delegationSecretHeader); got != "delegation-secret" {
			t.Errorf("delegation secret = %q", got)
		}
		var request delegationRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		switch calls.Add(1) {
		case 1:
			if request.SubjectToken != subjectToken || request.RootRequestID != "root-request" || request.GrantToken != "" {
				t.Errorf("unexpected initial request: %#v", request)
			}
			writeDelegationResponse(t, w, "child-1", "grant-1", now.Add(90*time.Minute), now.Add(4*time.Hour))
		case 2:
			if request.GrantToken != "grant-1" || request.SubjectToken != "" || request.RootRequestID != "" {
				t.Errorf("unexpected refresh request: %#v", request)
			}
			writeDelegationResponse(t, w, "child-2", "grant-2", now.Add(3*time.Hour), now.Add(6*time.Hour))
		default:
			t.Errorf("unexpected delegation call %d", calls.Load())
		}
	}))
	defer server.Close()

	em := &EnclaveManager{
		controlPlaneURL:           server.URL,
		inferenceDelegationSecret: "delegation-secret",
		delegationHTTPClient:      server.Client(),
	}
	provider, err := em.NewDelegatedAuthorizationProvider(context.Background(), subjectToken, "root-request")
	if err != nil {
		t.Fatalf("initial exchange: %v", err)
	}
	delegated := provider.(*delegatedAuthorization)
	delegated.now = func() time.Time { return now }
	if got, err := provider.Authorization(context.Background()); err != nil || got != "Bearer child-1" {
		t.Fatalf("initial authorization = %q, %v", got, err)
	}
	now = now.Add(2 * time.Hour)
	if got, err := provider.Authorization(context.Background()); err != nil || got != "Bearer child-2" {
		t.Fatalf("refreshed authorization = %q, %v", got, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("delegation calls = %d, want 2", got)
	}
}

func TestDelegatedAuthorizationShortLivedRefreshSingleflight(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			writeDelegationResponse(t, w, "child-1", "grant-1", now.Add(30*time.Minute), now.Add(4*time.Hour))
			return
		}
		if call == 2 {
			close(refreshStarted)
			<-releaseRefresh
			writeDelegationResponse(t, w, "child-2", "grant-2", now.Add(30*time.Second), now.Add(6*time.Hour))
			return
		}
		t.Errorf("unexpected delegation call %d", call)
	}))
	defer server.Close()

	em := &EnclaveManager{controlPlaneURL: server.URL, inferenceDelegationSecret: "secret", delegationHTTPClient: server.Client()}
	provider, err := em.NewDelegatedAuthorizationProvider(context.Background(), typedAccessJWT(t), "root")
	if err != nil {
		t.Fatal(err)
	}
	delegated := provider.(*delegatedAuthorization)
	now = now.Add(time.Hour)
	delegated.now = func() time.Time { return now }

	const callers = 20
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			authorization, err := provider.Authorization(context.Background())
			if err == nil && authorization != "Bearer child-2" {
				err = fmt.Errorf("authorization = %q", authorization)
			}
			errs <- err
		}()
	}
	close(start)
	<-refreshStarted
	close(releaseRefresh)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("delegation calls = %d, want initial plus one refresh", got)
	}
	for range 3 {
		if authorization, err := provider.Authorization(context.Background()); err != nil || authorization != "Bearer child-2" {
			t.Fatalf("short-lived authorization = %q, %v", authorization, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("short-lived token caused repeated refreshes: calls = %d", got)
	}
}

func TestDelegatedAuthorizationCanceledWaiterDoesNotCancelRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var calls atomic.Int32
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(refreshStarted)
		<-releaseRefresh
		writeDelegationResponse(t, w, "child", "grant", now.Add(time.Hour), now.Add(2*time.Hour))
	}))
	defer controlPlane.Close()
	provider := &delegatedAuthorization{
		client:   controlPlane.Client(),
		endpoint: controlPlane.URL,
		secret:   "secret",
		now:      func() time.Time { return now },
		tokens: delegationResponse{
			AccessToken:          "expired",
			AccessTokenExpiresAt: now,
			GrantToken:           "grant",
			GrantExpiresAt:       now.Add(2 * time.Hour),
		},
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	canceledResult := make(chan error, 1)
	go func() {
		_, err := provider.Authorization(canceledCtx)
		canceledResult <- err
	}()
	<-refreshStarted
	cancel()
	if err := <-canceledResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}

	healthyResult := make(chan error, 1)
	go func() {
		authorization, err := provider.Authorization(context.Background())
		if err == nil && authorization != "Bearer child" {
			err = fmt.Errorf("authorization = %q", authorization)
		}
		healthyResult <- err
	}()
	close(releaseRefresh)
	if err := <-healthyResult; err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestValidateDelegationControlPlaneURL(t *testing.T) {
	if err := validateDelegationControlPlaneURL("https://api.tinfoil.sh", false); err != nil {
		t.Fatalf("HTTPS production URL rejected: %v", err)
	}
	if err := validateDelegationControlPlaneURL("http://api.tinfoil.sh", false); err == nil {
		t.Fatal("expected production HTTP URL to be rejected")
	}
	if err := validateDelegationControlPlaneURL("http://127.0.0.1:8080", true); err != nil {
		t.Fatalf("debug local URL rejected: %v", err)
	}
}

func TestNewEnclaveManagerRejectsHTTPControlPlane(t *testing.T) {
	_, err := NewEnclaveManager(nil, "http://api.tinfoil.sh", "reporter", "reporting-secret", "context-secret", "delegation-secret", "", "", time.Minute, false)
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("constructor error = %v, want HTTPS validation error", err)
	}
}

func TestDelegationHTTPErrorPreservesStatusAndRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"access_token":"must-not-leak"}`))
	}))
	defer server.Close()
	provider := &delegatedAuthorization{client: server.Client(), endpoint: server.URL, secret: "secret", now: time.Now}

	_, err := provider.exchange(context.Background(), delegationRequest{GrantToken: "grant"})
	var httpErr *DelegationHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error type = %T, want *DelegationHTTPError", err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests || httpErr.RetryAfter != "42" {
		t.Fatalf("delegation HTTP error = %#v", httpErr)
	}
	if strings.Contains(err.Error(), "must-not-leak") {
		t.Fatal("delegation error leaked response body")
	}
}

func TestPostToEnclaveDelegationFailureBlocksDownstream(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer controlPlane.Close()
	provider := &delegatedAuthorization{
		client:   controlPlane.Client(),
		endpoint: controlPlane.URL,
		secret:   "secret",
		now:      func() time.Time { return now },
		tokens: delegationResponse{
			AccessToken:          "expired-child",
			AccessTokenExpiresAt: now,
			GrantToken:           "grant",
			GrantExpiresAt:       now.Add(time.Hour),
		},
	}
	var downstreamCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		downstreamCalls.Add(1)
	}))
	defer server.Close()
	enclave := &Enclave{host: strings.TrimPrefix(server.URL, "https://"), modelName: "test-model"}
	headers := http.Header{"Authorization": []string{"Bearer original"}}
	ctx := WithAuthorizationProvider(context.Background(), provider)

	if _, err := postToEnclave(ctx, server.Client(), enclave, "/v1/chat/completions", []byte("{}"), headers, nil); err == nil {
		t.Fatal("expected delegated authorization failure")
	}
	if got := downstreamCalls.Load(); got != 0 {
		t.Fatalf("downstream calls = %d, want 0", got)
	}
	if got := headers.Get("Authorization"); got != "Bearer original" {
		t.Fatalf("original headers changed to %q", got)
	}
}
