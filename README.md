# Confidential Inference Router

Tinfoil's confidential inference model router terminates TLS connections (optionally with EHBP), inspects the model name, and directs it to a verified secure inference enclave.

## Tool Calling

Client side tool calling is handled by the client. However, certain tools are provided server side. Right now this is only websearch.

To handle prompting, we add some instructions about how to call the websearch tools in the system prompt, and we have a short description in each tool.
_vLLM handles putting the system prompt + the tool prompts together, using internal templates built for the specific models._

## Tests & Evals

Unit / mock integration tests are colocated.

Bigger tests are in tests/. Also evaluations. Many of the models have different ways of doing things.
To get empiricial results on what models like doing what, it's encouraged to build out a simple eval in tests/

### CitationEval

1. Harmony is ~ [N†L1-L3]. This makes it different than all other citation techniques, which don't return line levels.
   1. The openAI API spec doesn't return this though, so neither do we. Just the standard url citation.
2. Sometimes harmony fails to put lines. We relax the regex to ~ [N†]
