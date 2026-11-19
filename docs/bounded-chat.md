# Bounded single-call chat (draft)

Bounded mode on `POST /v1/chat/completions` requires
`X-Tinfoil-Require-Single-Call: true`, positive `X-Tinfoil-Max-Input-Tokens`
and `X-Tinfoil-Max-Output-Tokens` headers, and `X-Tinfoil-Completion-Count: 1`.
Ordinary chat is unchanged. No production backend is enabled for bounded mode.

Bounded mode accepts text and OpenAI `image_url` parts on user messages. An image
URL must be an inline `data:image/png;base64,...` or `data:image/jpeg;base64,...`
URL with standard base64 and the matching image signature. `detail` may be
omitted or set to `auto`, `low`, or `high`. Remote URLs are intentionally out of
scope: fetching them adds network work and the content at a URL can change.
The backend, not the router, decodes and processes the image. An eligible image
backend must enforce the cap on the fully rendered prompt, including image
tokens used for billing, before generation. The router does not estimate image
tokens from the bytes or dimensions.

Output remains text-only. Files, audio, video, server-side tools, and replica
replays are not supported in bounded mode. Client-side function tool calls and
text-only tool results are permitted.
