# Confidential Inference Router

Tinfoil's confidential inference model router terminates TLS connections (optionally with EHBP), inspects the model name, and directs it to a verified secure inference enclave.

## Request bodies

On the OpenAI-compatible endpoints (`/v1/chat/completions`, `/v1/responses`) the router accepts two body shapes:

1. **Plain OpenAI**

2. **Tinfoil-wrapped envelope**

   ```json
   {
     "tinfoil_ctx": {
       "accessToken": "...",
       "encryptionKey": "...",
       "containerAuthToken": "..."
     },
     "payload": {
       /* the normal OpenAI body */
     }
   }
   ```

   The router lifts `tinfoil_ctx` out at the edge

## Tool Calling

Client side tool calling is handled by the client. Server-side tools currently supported: **web search** and **code execution**.

To handle prompting, we add some instructions about how to call each tool in the system prompt, and we have a short description in each tool.

_vLLM handles putting the system prompt + the tool prompts together, using internal templates built for the specific models._
