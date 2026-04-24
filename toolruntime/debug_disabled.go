//go:build !toolruntime_debug

package toolruntime

import (
	"net/http"
	"time"
)

// debugEnabled is a compile-time false constant in production builds. Every
// `if debugEnabled { ... }` block and every call whose body is purely
// `if !debugEnabled { return ... }` collapses at compile time, so the
// tracing helpers below carry zero runtime cost and the heavyweight
// argument expressions (debugPreview, debugMessagesSummary, json.Marshal
// fallbacks) are eliminated by the Go compiler's dead-code pass.
const debugEnabled = false

func debugTraceID() string { return "" }

func debugLogf(string, ...any) {}

func debugPreview(any, int) string { return "" }

func debugMessagesSummary([]any, int) string { return "" }

// ---------------------------------------------------------------------------
// devLog — no-op stub for production builds
// ---------------------------------------------------------------------------

// devLog is a no-op stub compiled into production binaries (no
// toolruntime_debug build tag). Every method is a no-op so call sites
// compile unconditionally without any #ifdef ceremony. The real
// implementation lives in debug_enabled.go and is only included in
// debug builds.
type devLog struct{}

func openDevLog(*http.Request, map[string]any, string, *sessionRegistry) *devLog      { return nil }
func (d *devLog) Close()                                                              {}
func (d *devLog) WriteTurnHeader(int)                                                 {}
func (d *devLog) WriteTokens(map[string]any)                                          {}
func (d *devLog) WriteResponseBody(map[string]any)                                    {}
func (d *devLog) WriteStreamedThinkingAndContent(string, string)                      {}
func (d *devLog) WriteToolCalls([]toolCall)                                           {}
func (d *devLog) WriteToolExec(string, map[string]any, string, time.Duration, string) {}
func (d *devLog) WriteFinish(string)                                                  {}
