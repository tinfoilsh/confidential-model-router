package manager

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
)

var errRetryReplica = errors.New("retry another replica")

type replicaRetryKey struct{}

type retryResponseWriter struct {
	http.ResponseWriter
	committed atomic.Bool
}

func (w *retryResponseWriter) WriteHeader(status int) {
	w.committed.Store(true)
	w.ResponseWriter.WriteHeader(status)
}

func (w *retryResponseWriter) Write(p []byte) (int, error) {
	w.committed.Store(true)
	return w.ResponseWriter.Write(p)
}

func (w *retryResponseWriter) Flush() {
	w.committed.Store(true)
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *retryResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ServeHTTPWithFailover retries one backend HTTP 502 before any response is
// committed. next must reserve a different eligible enclave, or return nil.
// It may update the replay's context for the new landing pool.
func (e *Enclave) ServeHTTPWithFailover(w http.ResponseWriter, r *http.Request, next func(*http.Request) *Enclave) {
	eligible := r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions" || r.URL.Path == "/v1/responses"
	if !eligible || r.Method != http.MethodPost || r.GetBody == nil {
		e.ServeHTTP(w, r)
		return
	}
	writer := &retryResponseWriter{ResponseWriter: w}
	var alternate *Enclave
	var replay *http.Request
	prepare := func() bool {
		if writer.committed.Load() || r.Context().Err() != nil {
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
	ctx := context.WithValue(r.Context(), replicaRetryKey{}, prepare)
	e.ServeHTTP(writer, r.WithContext(ctx))
	if alternate != nil {
		defer alternate.ReleaseDispatch()
		defer replay.Body.Close()
		alternate.ServeHTTP(w, replay)
	}
}
