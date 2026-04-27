package citations

// Tell model how to do citations

const Instructions = "Attach sources by embedding a clickable markdown link to the original URL directly after the sentence it supports. Format every citation exactly like this example, copying the punctuation characters verbatim: The sky is blue [Example page](https://example.com/article). The opening bracket is ASCII 0x5B, the closing bracket is ASCII 0x5D, and the URL is wrapped in ASCII parentheses 0x28 and 0x29. Reference 1-2 sources per claim; do not reference every source on every sentence. Copy the URL character-for-character from the tool output: preserve or omit a trailing slash exactly as the tool emitted it, keep query parameters verbatim, and do not append punctuation, whitespace, zero-width characters, or any other character after the URL before the closing parenthesis. Never invent URLs, never paraphrase URLs, and never wrap the link in any other brackets, braces, or quotation marks."

const HarmonyInstructions = "Each search result is numbered with a cursor like [1], [2], etc. and its content is split into lines. When you cite a source, use the Harmony citation format: 【cursor†Lstart-Lend】 where cursor is the result number and Lstart-Lend is the line range. For example, 【3†L5-L8】 cites lines 5 through 8 of result 3. For a single line, use 【2†L4】. Reference 1-2 sources per claim. Never invent citations."
