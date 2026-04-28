# Confidential Inference Router

Tinfoil's confidential inference model router terminates TLS connections (optionally with EHBP), inspects the model name, and directs it to a verified secure inference enclave.

## Tool Calling

Client side tool calling is handled by the client. However, certain tools are provided server side. Right now this is only websearch.

To handle prompting, we add some instructions about how to call the websearch tools in the system prompt, and we have a short description in each tool.
_vLLM handles putting the system prompt + the tool prompts together, using internal templates built for the specific models._
