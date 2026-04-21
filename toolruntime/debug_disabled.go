//go:build !toolruntime_debug

package toolruntime

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
