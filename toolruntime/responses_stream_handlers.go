package toolruntime

import "strings"

// captureIdentity records the upstream-provided response id, creation
// time and model on the first event that carries each one. Subsequent
// iterations retain the first captured value per field so the client sees
// one stable logical stream across every tool turn, and so the
// synthesized response.created and response.completed events carry
// upstream's real created_at instead of the router's wall clock.
func (s *responsesStreamer) captureIdentity(event map[string]any) {
	response, _ := event["response"].(map[string]any)
	if response == nil {
		return
	}
	if !s.upstreamIDCaptured {
		if id := stringValue(response["id"]); id != "" {
			s.responseID = id
			s.upstreamIDCaptured = true
		}
	}
	if s.createdAt == 0 {
		if created := int64(numberValue(response["created_at"])); created > 0 {
			s.createdAt = created
		}
	}
	if s.model == "" {
		if model := stringValue(response["model"]); model != "" {
			s.model = model
		}
	}
}

// ensureResponseCreated issues the synthesized response.created event
// using the streamer's stable identity fields, exactly once per logical
// stream. Callers may invoke it from the first upstream `response.created`
// frame (in which case the identity accessors return upstream's values)
// or from finalize (defensive, for upstreams that never send one). The
// accessors fall back to router-minted defaults only when strictly
// needed.
func (s *responsesStreamer) ensureResponseCreated() {
	if s.responseCreatedEmitted {
		return
	}
	s.responseCreatedEmitted = true
	s.emitEvent("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         s.streamResponseID(),
			"object":     "response",
			"created_at": s.streamCreatedAt(),
			"status":     "in_progress",
			"model":      s.streamModel(),
			"output":     []any{},
		},
	})
}

// handleOutputItemAdded forwards the event to the client after remapping
// output_index to the streamer's monotonically increasing namespace, or
// suppresses it entirely if the item represents a router-owned tool call
// we will execute ourselves. Item accumulation happens on the matching
// `_done` event so this handler does not need the iteration result.
//
// Client-owned function_call items are forwarded live so the calling SDK
// can act on them (e.g., execute the tool on its side and post results).
// Only tool names listed in s.ownedTools are suppressed; unknown tools
// default to client-owned to match the non-streaming splitToolCalls
// classification.
func (s *responsesStreamer) handleOutputItemAdded(event map[string]any) {
	item, _ := event["item"].(map[string]any)
	upstreamIndex := int(numberValue(event["output_index"]))
	if s.isRouterOwnedToolItem(item) {
		// Router-owned tool call; intercept for later resolution and do
		// not surface the intermediate tool invocation to the client.
		// mcp_call is included because some upstreams model the same
		// function invocation under the MCP output shape when tools are
		// presented via the legacy MCP path, and we want both to be
		// resolved identically.
		s.suppressedItems[upstreamIndex] = struct{}{}
		return
	}
	clientIndex := s.outputIndex
	s.outputIndexMap[upstreamIndex] = clientIndex
	s.outputIndex++
	payload := cloneJSONMap(event)
	payload["output_index"] = clientIndex
	s.emitEvent("response.output_item.added", payload)
}

// isRouterOwnedToolItem reports whether the given output item is a tool
// invocation this router will execute internally. Any function_call or
// mcp_call whose name is in the configured ownedTools set is suppressed
// from the client stream; everything else (including function_calls for
// tools the caller declared themselves) is forwarded verbatim.
func (s *responsesStreamer) isRouterOwnedToolItem(item map[string]any) bool {
	if item == nil {
		return false
	}
	itemType := stringValue(item["type"])
	if itemType != "function_call" && itemType != "mcp_call" {
		return false
	}
	if s.ownedTools == nil {
		return false
	}
	_, ok := s.ownedTools[stringValue(item["name"])]
	return ok
}

// handleOutputItemDone completes the client-facing view of an item and
// appends the finished item to finalOutput so response.completed can echo
// every output the model produced. For BOTH router-owned and client-owned
// function calls the parsed arguments are recorded for the loop; when the
// upstream `done` event lacks arguments (some servers ship them only via
// the delta stream) we fall back to the per-index buffer accumulated from
// `response.function_call_arguments.delta` frames. For client-owned
// calls the rebuilt arguments string is also written back into the item
// so the forwarded `output_item.done` event and the terminal
// `response.completed.output` both carry a usable tool call.
func (s *responsesStreamer) handleOutputItemDone(event map[string]any, result *responsesIterationResult) {
	item, _ := event["item"].(map[string]any)
	upstreamIndex := int(numberValue(event["output_index"]))
	itemType := stringValue(item["type"])
	if s.isRouterOwnedToolItem(item) {
		arguments := parseArguments(item["arguments"])
		if len(arguments) == 0 {
			if buf, ok := s.functionCallArguments[upstreamIndex]; ok {
				arguments = parseArguments(buf.String())
				// Splice the reassembled JSON back into the item before
				// it lands in result.outputItems. The outer loop replays
				// those items into the next /v1/responses turn's input,
				// and some upstream variants ship arguments only via
				// delta frames -- without this splice the replayed item
				// would carry an empty arguments string and the model
				// would see its own prior tool call as argument-less.
				if rawArgs := stringValue(item["arguments"]); rawArgs == "" {
					item["arguments"] = buf.String()
				}
			}
		}
		delete(s.functionCallArguments, upstreamIndex)
		result.outputItems = append(result.outputItems, item)
		callID := stringValue(item["call_id"])
		if callID == "" {
			callID = stringValue(item["id"])
		}
		result.toolCalls = append(result.toolCalls, toolCall{
			id:        callID,
			name:      stringValue(item["name"]),
			arguments: arguments,
		})
		return
	}
	if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
		return
	}
	// Client-owned function_calls: reassemble arguments from the delta
	// buffer when the item itself ships with an empty arguments string,
	// and splice the reassembled form back into the item so downstream
	// consumers see a complete tool call.
	if itemType == "function_call" || itemType == "mcp_call" {
		if rawArgs := stringValue(item["arguments"]); rawArgs == "" {
			if buf, ok := s.functionCallArguments[upstreamIndex]; ok && buf.Len() > 0 {
				item["arguments"] = buf.String()
			}
		}
		delete(s.functionCallArguments, upstreamIndex)
	}
	clientIndex, ok := s.outputIndexMap[upstreamIndex]
	if !ok {
		clientIndex = s.outputIndex
		s.outputIndexMap[upstreamIndex] = clientIndex
		s.outputIndex++
	}
	payload := cloneJSONMap(event)
	payload["output_index"] = clientIndex
	s.emitEvent("response.output_item.done", payload)
	s.finalOutput = append(s.finalOutput, item)
	result.outputItems = append(result.outputItems, item)
	if itemType == "function_call" || itemType == "mcp_call" {
		// Client-owned tool call: propagate to iteration result so
		// splitToolCalls classifies it as a client tool call and the
		// outer loop falls through to finalize rather than attempting to
		// execute it internally.
		callID := stringValue(item["call_id"])
		if callID == "" {
			callID = stringValue(item["id"])
		}
		result.toolCalls = append(result.toolCalls, toolCall{
			id:        callID,
			name:      stringValue(item["name"]),
			arguments: parseArguments(item["arguments"]),
		})
	}
}

// handleFunctionCallArgumentsDelta accumulates partial JSON arguments for
// a router-owned function_call item that the loop will resolve internally.
// The buffered string is consumed when the matching
// `response.output_item.done` event fires.
func (s *responsesStreamer) handleFunctionCallArgumentsDelta(event map[string]any) {
	upstreamIndex := int(numberValue(event["output_index"]))
	delta := stringValue(event["delta"])
	if delta == "" {
		return
	}
	if s.functionCallArguments == nil {
		s.functionCallArguments = map[int]*strings.Builder{}
	}
	buf, ok := s.functionCallArguments[upstreamIndex]
	if !ok {
		buf = &strings.Builder{}
		s.functionCallArguments[upstreamIndex] = buf
	}
	buf.WriteString(delta)
}

// handleOutputTextDelta pushes the incoming text fragment through the
// content's citation emitter so inline markdown links resolve to
// annotation events, and forwards remaining text to the client as an
// output_text.delta event.
func (s *responsesStreamer) handleOutputTextDelta(event map[string]any) {
	upstreamIndex := int(numberValue(event["output_index"]))
	if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
		return
	}
	clientIndex, ok := s.outputIndexMap[upstreamIndex]
	if !ok {
		clientIndex = s.outputIndex
		s.outputIndexMap[upstreamIndex] = clientIndex
		s.outputIndex++
	}
	itemID := stringValue(event["item_id"])
	contentIndex := int(numberValue(event["content_index"]))
	key := itemContentKey{itemID: itemID, contentIndex: contentIndex}
	emitter, exists := s.emitters[key]
	if !exists {
		emitter = newCitationEmitter(s.citations)
		s.emitters[key] = emitter
	}
	delta := stringValue(event["delta"])
	contentChunk, annotations := emitter.push(delta)
	if contentChunk != "" {
		s.emitEvent("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"output_index":  clientIndex,
			"item_id":       itemID,
			"content_index": contentIndex,
			"delta":         contentChunk,
		})
	}
	s.emitAnnotationEvents(annotations, clientIndex, itemID, contentIndex)
}

// handleOutputTextDone flushes any content still held in the emitter's
// trailing buffer so the user sees every rune before the item closes, then
// forwards the done event with identity rewritten.
func (s *responsesStreamer) handleOutputTextDone(event map[string]any) {
	upstreamIndex := int(numberValue(event["output_index"]))
	if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
		return
	}
	clientIndex, ok := s.outputIndexMap[upstreamIndex]
	if !ok {
		clientIndex = s.outputIndex
		s.outputIndexMap[upstreamIndex] = clientIndex
		s.outputIndex++
	}
	itemID := stringValue(event["item_id"])
	contentIndex := int(numberValue(event["content_index"]))
	key := itemContentKey{itemID: itemID, contentIndex: contentIndex}
	if emitter, ok := s.emitters[key]; ok {
		contentChunk, annotations := emitter.flush()
		if contentChunk != "" {
			s.emitEvent("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"output_index":  clientIndex,
				"item_id":       itemID,
				"content_index": contentIndex,
				"delta":         contentChunk,
			})
		}
		s.emitAnnotationEvents(annotations, clientIndex, itemID, contentIndex)
		delete(s.emitters, key)
	}
	payload := cloneJSONMap(event)
	payload["output_index"] = clientIndex
	s.emitEvent("response.output_text.done", payload)
}

// emitAnnotationEvents turns resolved link matches into the documented
// Responses annotation event shape: `response.output_text.annotation.added`
// with a flat url_citation attached to the item/content coordinates the
// matching text lives at. The `annotation_index` is a monotonic cursor
// per (item_id, content_index) pair so clients can use it to dedupe or
// order annotations across the full text stream, not just within the
// single push/flush batch that happened to resolve them.
func (s *responsesStreamer) emitAnnotationEvents(matches []citationAnnotation, outputIndex int, itemID string, contentIndex int) {
	if len(matches) == 0 {
		return
	}
	if s.annotationCounts == nil {
		s.annotationCounts = map[itemContentKey]int{}
	}
	key := itemContentKey{itemID: itemID, contentIndex: contentIndex}
	for _, match := range matches {
		annotation := map[string]any{
			"type":        "url_citation",
			"url":         match.source.url,
			"start_index": match.startIndex,
			"end_index":   match.endIndex,
		}
		if match.source.title != "" {
			annotation["title"] = match.source.title
		}
		annotationIndex := s.annotationCounts[key]
		s.annotationCounts[key] = annotationIndex + 1
		s.emitEvent("response.output_text.annotation.added", map[string]any{
			"type":             "response.output_text.annotation.added",
			"output_index":     outputIndex,
			"item_id":          itemID,
			"content_index":    contentIndex,
			"annotation_index": annotationIndex,
			"annotation":       annotation,
		})
	}
}

// forwardEvent relays an upstream event the streamer does not specially
// handle, rewriting output_index when present so the client always sees
// the streamer's canonical monotonic namespace.
func (s *responsesStreamer) forwardEvent(eventType string, event map[string]any) {
	if rawIndex, ok := event["output_index"]; ok {
		upstreamIndex := int(numberValue(rawIndex))
		if _, suppressed := s.suppressedItems[upstreamIndex]; suppressed {
			return
		}
		if clientIndex, mapped := s.outputIndexMap[upstreamIndex]; mapped {
			event = cloneJSONMap(event)
			event["output_index"] = clientIndex
		}
	}
	s.emitEvent(eventType, event)
}
