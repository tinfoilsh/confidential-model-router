# Visible completion count for encrypted requests

`X-Tinfoil-Completion-Count` optionally sets `n` on `/v1/chat/completions`,
replacing any value in the decrypted body. Like `X-Tinfoil-Max-Output-Tokens`,
it remains readable to an EHBP proxy while the request body stays encrypted.

```http
X-Tinfoil-Max-Output-Tokens: 100
X-Tinfoil-Completion-Count: 3
```

The enclave forwards `n: 3` and `max_completion_tokens: 100`, regardless of the
body's original values. For a single model call this permits at most three
choices of up to 100 output tokens each, not 100 tokens shared across choices.

The count must be one positive signed-64-bit base-10 integer after whitespace
trimming, following the output-token header's parsing conventions. Empty,
non-integer, non-positive, overflowing, comma-separated, or repeated values
return 400. Backend-specific count limits may still reject a request.

When the count header is present, `best_of` must be omitted or the integer `1`;
other values return 400, because extra candidates could hide additional work.
A validated `best_of: 1` is removed before forwarding to avoid conflicting
with an enforced count larger than one.

Without the count header, `n` and `best_of` retain their existing behavior,
even if the output-token header is supplied. Other endpoints ignore the count
header. This feature does not change Responses or legacy Completions.

A proxy may supply count `1` when the client supplies no visible count. Any
reservation must use the exact enforced headers forwarded to the enclave and
require a deployed enclave version supporting them. These headers are not
integrity-bound to the encrypted body and do not bound aggregate output across
server-side tool loops, retries, or repeated model calls.
