package providers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/anthropic"
	"github.com/whtsky/copilot2api/internal/modelid"
	"github.com/whtsky/copilot2api/internal/sse"
)

// Route determines the handler for an incoming request based on model prefix and endpoint.
func (c *Config) Route(r *http.Request) (http.Handler, error) {
	body, model, err := readBodyAndModel(r)
	if err != nil {
		return nil, err
	}
	endpoint := r.URL.Path

	// ── copilot-* → capability-driven routing ──
	if strings.HasPrefix(model, "copilot-") {
		stripped, newBody := stripPrefix(body, model, "copilot-")
		// Drop UI-only display suffix (e.g. "[1m]").
		if clean := modelid.StripDisplayContextSuffixes(stripped); clean != stripped {
			newBody = rewriteModel(newBody, stripped, clean)
			stripped = clean
		}
		// Strip Anthropic fields Copilot does not support.
		newBody = stripCopilotFields(newBody)
		r.Body = io.NopCloser(bytes.NewReader(newBody))
		r.ContentLength = int64(len(newBody))
		slog.Debug("routing to copilot", "model", model, "stripped", stripped)

		// Per-model endpoint capabilities from the Copilot /models cache.
		// Fall back to heuristic when cache is cold or model is missing:
		// "gpt-*" models are OpenAI-native (/responses), others are Claude-native (/messages).
		eps, ok := c.copilotModels[stripped]
		if !ok || c.copilotModels == nil {
			eps = copilotEndpoints{
				supportsMessages:  !strings.HasPrefix(stripped, "gpt-"),
				supportsResponses: strings.HasPrefix(stripped, "gpt-"),
			}
		}

		switch endpoint {
		case "/v1/messages":
			if eps.supportsMessages {
				return c.copilotHandler, nil // native passthrough (Claude)
			}
			if eps.supportsResponses && c.copilotAnthropicHandler != nil {
				return c.copilotAnthropicHandler, nil // Messages→Responses (GPT)
			}
			// Unknown model — keep backward compatibility.
			if c.copilotAnthropicHandler != nil {
				return c.copilotAnthropicHandler, nil
			}
		case "/v1/responses":
			if eps.supportsResponses {
				return c.copilotHandler, nil // native passthrough (GPT)
			}
			if eps.supportsMessages {
				return c.makeCopilotResponsesToMessagesHandler(), nil // Responses→Messages (Claude)
			}
		}

		return c.copilotHandler, nil // fallback
	}

	// ── Find provider ──
	p := c.FindProvider(model)
	if p == nil {
		return nil, fmt.Errorf("unknown model: %s", model)
	}

	// ── Strip provider prefix from model ──
	stripped, newBody := stripPrefix(body, model, p.ModelPrefix+"-")
	_ = stripped // used in logs, the important part is the modified body
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))

	// ── Native format → passthrough ──
	switch {
	case endpoint == "/v1/messages" && p.Type == "anthropic":
		return p.makePassthroughHandler("/v1/messages"), nil
	case endpoint == "/v1/responses" && p.Type == "openai":
		return p.makePassthroughHandler("/v1/responses"), nil
	case endpoint == "/v1/chat/completions" && (p.Type == "openai" || p.Type == "chat"):
		return p.makePassthroughHandler("/v1/chat/completions"), nil
	}

	// ── Conversion ──
	switch {
	case endpoint == "/v1/messages" && p.Type == "openai" && p.ConvertToAnthropic:
		return p.makeMessagesToResponsesHandler(), nil
	case endpoint == "/v1/responses" && p.Type == "anthropic" && p.ConvertToOpenAI:
		return p.makeResponsesToAnthropicHandler(), nil
	case endpoint == "/v1/messages" && p.Type == "chat" && p.ConvertToAnthropic:
		return p.makeMessagesToChatHandler(), nil
	}

	return nil, fmt.Errorf("model %s does not support endpoint %s", model, endpoint)
}

// ServeHTTP implements http.Handler for Config-based routing.
func (c *Config) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	endpoint := r.URL.Path

	switch endpoint {
	case "/v1/messages", "/v1/responses", "/v1/chat/completions":
		handler, err := c.Route(r)
		if err != nil {
			writeError(w, endpoint, http.StatusBadRequest, err.Error())
			return
		}
		handler.ServeHTTP(w, r)
	default:
		// Passthrough to copilot handler for /v1/embeddings etc.
		c.copilotHandler.ServeHTTP(w, r)
	}
}

// makePassthroughHandler creates a reverse proxy handler for the given upstream endpoint.
func (p *ProviderConfig) makePassthroughHandler(upstreamEndpoint string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := loggerFromContext(r.Context())
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusBadRequest, "failed to read request body")
			return
		}

		// Apply adapter patches.
		if adapter := GetAdapter(p.ProviderID); adapter != nil {
			var reqMap map[string]interface{}
			if err := json.Unmarshal(body, &reqMap); err == nil {
				reqMap = adapter.PatchRequest(reqMap)
				body, _ = json.Marshal(reqMap)
			}
		}

		targetURL := strings.TrimRight(p.BaseURL, "/") + upstreamEndpoint
		proxyReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(body))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to create upstream request")
			return
		}
		copyHeaders(proxyReq.Header, r.Header)
		proxyReq.Header.Set("Content-Type", "application/json")
		proxyReq.Header.Set("User-Agent", "aiproxy/1.0.0")
		if err := p.setAuthHeader(proxyReq); err != nil {
			writeError(w, r.URL.Path, http.StatusBadGateway, err.Error())
			return
		}

		client := p.httpClient()

		// Retry up to 3 times with exponential backoff.
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			resp, err := client.Do(proxyReq)
			if err == nil {
				defer resp.Body.Close()
				respBody, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
				if err != nil {
					writeError(w, r.URL.Path, http.StatusBadGateway, "failed to read upstream response")
					return
				}
				if resp.StatusCode >= 500 && attempt < 2 {
					resp.Body.Close()
					time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
					proxyReq.Body = io.NopCloser(bytes.NewReader(body))
					continue
				}
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				w.Write(respBody)
				return
			}
			lastErr = err
			if attempt < 2 {
				time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
			}
		}

		// All retries exhausted or fatal error.
		logger.Error("upstream request failed", "provider", p.ProviderID, "url", targetURL, "error", lastErr)
		writeError(w, r.URL.Path, http.StatusBadGateway, fmt.Sprintf("upstream unavailable: %v", lastErr))
	})
}

// makeCopilotResponsesToMessagesHandler converts OpenAI /v1/responses →
// Anthropic /v1/messages via linguafranca, proxies through the copilot handler
// (which does native /messages passthrough for Claude), and converts the
// response back to OpenAI Responses format.
func (c *Config) makeCopilotResponsesToMessagesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := loggerFromContext(r.Context())
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeError(w, "/v1/responses", http.StatusBadRequest, "failed to read request body")
			return
		}

		var reqMeta struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		json.Unmarshal(body, &reqMeta)

		// 1. Responses → Anthropic Messages via linguafranca.
		messagesBody, err := linguafrancaConvertRequest(body)
		if err != nil {
			logger.Error("linguafranca request conversion failed", "error", err)
			writeError(w, "/v1/responses", http.StatusBadGateway, "conversion failed")
			return
		}
		if len(messagesBody) > 10<<20 {
			writeError(w, "/v1/responses", http.StatusRequestEntityTooLarge,
				"request body too large after conversion")
			return
		}

		// Copilot /v1/messages does not accept cache_control; strip it.
		{
			var msgMap map[string]any
			if json.Unmarshal(messagesBody, &msgMap) == nil {
				if _, ok := msgMap["cache_control"]; ok {
					delete(msgMap, "cache_control")
					messagesBody, _ = json.Marshal(msgMap)
				}
			}
		}

		// 2. Build request to copilotHandler on /v1/messages (native passthrough).
		req, _ := http.NewRequestWithContext(r.Context(), "POST", "/v1/messages",
			bytes.NewReader(messagesBody))
		copyHeaders(req.Header, r.Header)
		req.Header.Set("Content-Type", "application/json")

		if reqMeta.Stream {
			// 3a. Streaming: io.Pipe instead of responseRecorder.
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				rec := &responseRecorder{header: make(http.Header)}
				c.copilotHandler.ServeHTTP(rec, req)
				if rec.statusCode != http.StatusOK && rec.statusCode != 0 {
					pw.Write(rec.body)
					return
				}
				pw.Write(rec.body)
			}()
			if err := linguafrancaConvertStream(w, pr); err != nil {
				logger.Error("linguafranca stream conversion failed", "error", err)
			}
			return
		}

		// 3b. Non-streaming: responseRecorder.
		rec := &responseRecorder{header: make(http.Header)}
		c.copilotHandler.ServeHTTP(rec, req)

		if rec.statusCode != http.StatusOK && rec.statusCode != 0 {
			forwardCopilotError(w, rec)
			return
		}

		// 4. Anthropic Messages → Responses via linguafranca.
		result, err := linguafrancaConvertResponse(rec.body)
		if err != nil {
			logger.Error("linguafranca response conversion failed", "error", err)
			writeError(w, "/v1/responses", http.StatusBadGateway,
				"response conversion failed")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(result)
	})
}

// forwardCopilotError forwards an error response from copilotHandler verbatim.
func forwardCopilotError(w http.ResponseWriter, rec *responseRecorder) {
	for k, vs := range rec.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.statusCode)
	w.Write(rec.body)
}

// makeResponsesToAnthropicHandler converts Responses → Anthropic Messages via linguafranca,
// proxies to the upstream provider, and converts the response back.
func (p *ProviderConfig) makeResponsesToAnthropicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := loggerFromContext(r.Context())
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusBadRequest, "failed to read request body")
			return
		}

		// Check streaming.
		var reqMeta struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		json.Unmarshal(body, &reqMeta)

		// Apply adapter patches before conversion.
		if adapter := GetAdapter(p.ProviderID); adapter != nil {
			var reqMap map[string]interface{}
			if err := json.Unmarshal(body, &reqMap); err == nil {
				reqMap = adapter.PatchRequest(reqMap)
				body, _ = json.Marshal(reqMap)
			}
		}

		// Convert Responses → Anthropic Messages via linguafranca.
		messagesBody, err := linguafrancaConvertRequest(body)
		if err != nil {
			logger.Error("linguafranca request conversion failed", "error", err, "provider", p.ProviderID, "model", reqMeta.Model)
			writeError(w, r.URL.Path, http.StatusBadGateway, "conversion failed")
			return
		}

		isStream := reqMeta.Stream

		// Build upstream request.
		targetURL := strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
		upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(messagesBody))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to create upstream request")
			return
		}
		copyHeaders(upstreamReq.Header, r.Header)
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("User-Agent", "aiproxy/1.0.0")
		if err := p.setAuthHeader(upstreamReq); err != nil {
			writeError(w, r.URL.Path, http.StatusBadGateway, err.Error())
			return
		}

		client := p.httpClient()

		if isStream {
			upstreamReq.Header.Set("Accept", "text/event-stream")
			resp, err := client.Do(upstreamReq)
			if err != nil {
				logger.Error("upstream streaming failed", "error", err)
				writeError(w, r.URL.Path, http.StatusBadGateway, "upstream unavailable")
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				w.Write(errBody)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			if err := linguafrancaConvertStream(w, resp.Body); err != nil {
				logger.Error("stream conversion failed", "error", err)
			}
			return
		}

		// Non-streaming.
		resp, err := client.Do(upstreamReq)
		if err != nil {
			logger.Error("upstream request failed", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "upstream unavailable")
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusBadGateway, "failed to read upstream response")
			return
		}

		if resp.StatusCode >= 400 {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			w.Write(respBody)
			return
		}

		// Convert Anthropic Messages → Responses.
		result, err := linguafrancaConvertResponse(respBody)
		if err != nil {
			logger.Error("linguafranca response conversion failed", "error", err, "provider", p.ProviderID)
			writeError(w, r.URL.Path, http.StatusBadGateway, "conversion failed")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(result)
	})
}

// makeMessagesToResponsesHandler converts Anthropic /v1/messages →
// OpenAI /v1/responses via Go converters (no linguafranca), proxies to
// the upstream provider, and converts the response back.
func (p *ProviderConfig) makeMessagesToResponsesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := loggerFromContext(r.Context())
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusBadRequest, "failed to read request body")
			return
		}

		var msgReq anthropic.AnthropicMessagesRequest
		if err := json.Unmarshal(body, &msgReq); err != nil {
			writeError(w, r.URL.Path, http.StatusBadRequest, "invalid JSON")
			return
		}

		responsesReq, err := anthropic.ConvertAnthropicToResponses(msgReq)
		if err != nil {
			logger.Error("messages→responses conversion failed", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "conversion failed")
			return
		}

		reqBody, err := json.Marshal(responsesReq)
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to marshal request")
			return
		}

		isStream := msgReq.Stream
		targetURL := strings.TrimRight(p.BaseURL, "/") + "/v1/responses"
		upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(reqBody))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to create upstream request")
			return
		}
		copyHeaders(upstreamReq.Header, r.Header)
		upstreamReq.Header.Set("Content-Type", "application/json")
		if err := p.setAuthHeader(upstreamReq); err != nil {
			writeError(w, r.URL.Path, http.StatusBadGateway, err.Error())
			return
		}

		client := p.httpClient()

		if isStream {
			upstreamReq.Header.Set("Accept", "text/event-stream")
			resp, err := client.Do(upstreamReq)
			if err != nil {
				logger.Error("upstream streaming failed", "error", err)
				writeError(w, r.URL.Path, http.StatusBadGateway, "upstream unavailable")
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				w.Write(errBody)
				return
			}

			state := &anthropic.ResponsesStreamState{}
			sse.BeginSSE(w)
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
			var eventType string
			hasFlusher, canFlush := w.(http.Flusher)
			_ = hasFlusher

			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "event: ") {
					eventType = strings.TrimPrefix(line, "event: ")
					continue
				}
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				dataStr := strings.TrimPrefix(line, "data: ")
				if dataStr == "[DONE]" {
					break
				}
				var sev anthropic.ResponseStreamEvent
				if err := json.Unmarshal([]byte(dataStr), &sev); err != nil {
					continue
				}
				if sev.Type == "" && eventType != "" {
					sev.Type = eventType
				}
				events := anthropic.TranslateResponsesStreamEvent(sev, state)
				for _, ev := range events {
					evData, _ := json.Marshal(ev)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, evData)
				}
				if canFlush && len(events) > 0 {
					flusher := w.(http.Flusher)
					flusher.Flush()
				}
			}
			return
		}

		// Non-streaming.
		resp, err := client.Do(upstreamReq)
		if err != nil {
			logger.Error("upstream request failed", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "upstream unavailable")
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusBadGateway, "failed to read upstream response")
			return
		}
		if resp.StatusCode >= 400 {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			w.Write(respBody)
			return
		}

		var result anthropic.ResponsesResult
		if err := json.Unmarshal(respBody, &result); err != nil {
			logger.Error("failed to parse responses result", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "failed to parse upstream response")
			return
		}

		msgResp := anthropic.ConvertResponsesToAnthropic(result)
		resultJSON, err := json.Marshal(msgResp)
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to marshal response")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(resultJSON)
	})
}

// makeMessagesToChatHandler converts Anthropic /v1/messages →
// /v1/chat/completions via Go converters, proxies to the upstream
// provider, and converts the response back.
func (p *ProviderConfig) makeMessagesToChatHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := loggerFromContext(r.Context())
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusBadRequest, "failed to read request body")
			return
		}

		var msgReq anthropic.AnthropicMessagesRequest
		if err := json.Unmarshal(body, &msgReq); err != nil {
			writeError(w, r.URL.Path, http.StatusBadRequest, "invalid JSON")
			return
		}

		chatReq, err := anthropic.ConvertAnthropicToOpenAI(msgReq)
		if err != nil {
			logger.Error("messages→chat conversion failed", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "conversion failed")
			return
		}

		reqBody, err := json.Marshal(chatReq)
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to marshal request")
			return
		}

		isStream := msgReq.Stream
		targetURL := strings.TrimRight(p.BaseURL, "/") + "/v1/chat/completions"
		upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(reqBody))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to create upstream request")
			return
		}
		copyHeaders(upstreamReq.Header, r.Header)
		upstreamReq.Header.Set("Content-Type", "application/json")
		if err := p.setAuthHeader(upstreamReq); err != nil {
			writeError(w, r.URL.Path, http.StatusBadGateway, err.Error())
			return
		}

		client := p.httpClient()

		if isStream {
			upstreamReq.Header.Set("Accept", "text/event-stream")
			resp, err := client.Do(upstreamReq)
			if err != nil {
				logger.Error("upstream streaming failed", "error", err)
				writeError(w, r.URL.Path, http.StatusBadGateway, "upstream unavailable")
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				w.Write(errBody)
				return
			}

			state := anthropic.NewStreamState()
			sse.BeginSSE(w)
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
			hasFlusher, canFlush := w.(http.Flusher)
			_ = hasFlusher

			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				dataStr := strings.TrimPrefix(line, "data: ")
				if dataStr == "[DONE]" {
					break
				}
				var chunk anthropic.OpenAIChatCompletionChunk
				if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
					continue
				}
				events, err := anthropic.ConvertOpenAIChunkToAnthropicEvents(chunk, state)
				if err != nil {
					logger.Error("chat chunk conversion failed", "error", err)
					continue
				}
				for _, ev := range events {
					evData, _ := json.Marshal(ev)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, evData)
				}
				if canFlush && len(events) > 0 {
					flusher := w.(http.Flusher)
					flusher.Flush()
				}
			}
			return
		}

		// Non-streaming.
		resp, err := client.Do(upstreamReq)
		if err != nil {
			logger.Error("upstream request failed", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "upstream unavailable")
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		if err != nil {
			writeError(w, r.URL.Path, http.StatusBadGateway, "failed to read upstream response")
			return
		}
		if resp.StatusCode >= 400 {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			w.Write(respBody)
			return
		}

		var chatResp anthropic.OpenAIChatCompletionsResponse
		if err := json.Unmarshal(respBody, &chatResp); err != nil {
			logger.Error("failed to parse chat response", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "failed to parse upstream response")
			return
		}

		msgResp, err := anthropic.ConvertOpenAIToAnthropic(chatResp)
		if err != nil {
			logger.Error("chat→messages conversion failed", "error", err)
			writeError(w, r.URL.Path, http.StatusBadGateway, "conversion failed")
			return
		}

		resultJSON, err := json.Marshal(msgResp)
		if err != nil {
			writeError(w, r.URL.Path, http.StatusInternalServerError, "failed to marshal response")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(resultJSON)
	})
}

func (p *ProviderConfig) setAuthHeader(req *http.Request) error {
	// Anthropic requires the version header regardless of auth mode (K4).
	if p.Type == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if tp := p.TokenProvider(); tp != nil {
		tok, err := tp.GetAccessToken()
		if err != nil {
			return fmt.Errorf("failed to get access token for %s: %w", p.ProviderID, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
	switch p.Type {
	case "anthropic":
		if p.AuthHeader == "bearer" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		} else {
			req.Header.Set("x-api-key", p.APIKey)
		}
	case "openai", "chat":
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	case "copilot":
		slog.Warn("copilot provider reached setAuthHeader — routing bug", "provider", p.ProviderID)
	}
	return nil
}

func (p *ProviderConfig) httpClient() *http.Client {
	return p.client
}

// copyHeaders copies headers from src to dst, skipping hop-by-hop headers.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		kl := strings.ToLower(k)
		if kl == "connection" || kl == "keep-alive" || kl == "proxy-connection" ||
			kl == "transfer-encoding" || kl == "upgrade" || kl == "trailer" ||
			kl == "x-api-key" || kl == "anthropic-version" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// NOTE: Body is read here for routing, then replaced via NopCloser.
// Downstream handlers (makePassthroughHandler, etc.) read it again.
// This is safe but adds a JSON decode+encode round-trip per request.
func readBodyAndModel(r *http.Request) ([]byte, string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return nil, "", fmt.Errorf("failed to read request body: %w", err)
	}

	var top struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return body, "", fmt.Errorf("failed to parse request JSON: %w", err)
	}
	if top.Model == "" {
		return body, "", fmt.Errorf("model field is required")
	}

	return body, top.Model, nil
}

// stripPrefix removes the given prefix from the model field in the JSON body.
// Returns the stripped model ID and the modified body.
func stripPrefix(body []byte, model, prefix string) (string, []byte) {
	stripped := strings.TrimPrefix(model, prefix)
	return stripped, rewriteModel(body, model, stripped)
}

// rewriteModel sets the JSON "model" field to newModel, tolerating whitespace
// formatting (UseNumber keeps large integers exact; SetEscapeHTML(false) avoids
// <-style escaping). Falls back to string replacement for unparseable bodies.
func rewriteModel(body []byte, oldModel, newModel string) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var parsed map[string]interface{}
	if err := dec.Decode(&parsed); err != nil {
		return bytes.Replace(body,
			[]byte(`"model":"`+oldModel+`"`),
			[]byte(`"model":"`+newModel+`"`),
			1,
		)
	}
	parsed["model"] = newModel
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(parsed); err != nil {
		return body
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// stripCopilotFields removes Anthropic-native fields that Copilot does not
// support: context_management (context window hints). Modifies in-place
// via JSON round-trip; returns original body on parse failure.
func stripCopilotFields(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	_, hasCM := m["context_management"]
	if !hasCM {
		return body
	}
	delete(m, "context_management")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// writeError writes a JSON error response in the appropriate protocol format.
func writeError(w http.ResponseWriter, endpoint string, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	switch {
	case endpoint == "/v1/messages":
		// Anthropic format.
		fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":%q}}`, message)
	case endpoint == "/v1/responses", endpoint == "/v1/chat/completions":
		// OpenAI format.
		fmt.Fprintf(w, `{"error":{"type":"api_error","message":%q}}`, message)
	default:
		fmt.Fprintf(w, `{"error":{"type":"api_error","message":%q}}`, message)
	}
}
