package manager

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync/atomic"
)

var errRetryReplica = errors.New("retry another replica")

type replicaRetryKey struct{}

// next reserves a different eligible enclave and may update the replay's routing labels.
func (e *Enclave) ServeHTTPWithFailover(w http.ResponseWriter, r *http.Request, next func(*http.Request) *Enclave) {
	eligible := r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions" || r.URL.Path == "/v1/responses"
	if !eligible || r.Method != http.MethodPost || r.GetBody == nil {
		e.ServeHTTP(w, r)
		return
	}
	// ModifyResponse runs before final headers; only 1xx can precede it.
	var interim atomic.Bool
	trace := &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error {
		interim.Store(true)
		return nil
	}}
	var alternate *Enclave
	var replay *http.Request
	prepare := func() bool {
		if interim.Load() || r.Context().Err() != nil {
			return false
		}
		body, err := r.GetBody()
		if err != nil {
			return false
		}
		replay = r.Clone(WithProbeClaim(r.Context(), nil))
		replay.Body = body
		alternate = next(replay)
		if alternate == nil {
			body.Close()
			return false
		}
		return true
	}
	ctx := context.WithValue(httptrace.WithClientTrace(r.Context(), trace), replicaRetryKey{}, prepare)
	e.ServeHTTP(w, r.WithContext(ctx))
	if alternate != nil {
		defer alternate.ReleaseDispatch()
		defer replay.Body.Close()
		alternate.ServeHTTP(w, replay)
	}
}
