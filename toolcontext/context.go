package toolcontext

const (
	HeaderRequestID      = "X-Tinfoil-Tool-Request-Id"
	HeaderModel          = "X-Tinfoil-Tool-Model"
	HeaderRoute          = "X-Tinfoil-Tool-Route"
	HeaderStreaming      = "X-Tinfoil-Tool-Streaming"
	HeaderPrincipalType  = "X-Tinfoil-Tool-Principal-Type"
	HeaderPrincipalID    = "X-Tinfoil-Tool-Principal-Id"
	HeaderOrgID          = "X-Tinfoil-Tool-Org-Id"
	HeaderPIICheck       = "X-Tinfoil-Tool-PII-Check"
	HeaderInjectionCheck = "X-Tinfoil-Tool-Injection-Check"

	// Code-execution per-request secrets
	HeaderCodeExecutionAccessToken        = "X-Code-Execution-Access-Token"
	HeaderCodeExecutionEncryptionKey      = "X-Code-Execution-Encryption-Key"
	HeaderCodeExecutionContainerAuthToken = "X-Code-Execution-Container-Auth-Token"
)
