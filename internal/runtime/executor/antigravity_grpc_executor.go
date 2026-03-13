package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/status"
)

// executeClaudeNonStreamGRPC performs a non-streaming request via gRPC GenerateChat.
func (e *AntigravityExecutor) executeClaudeNonStreamGRPC(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
	baseModel, token string, translated []byte, reporter *usageReporter, from, to sdktranslator.Format) (cliproxyexecutor.Response, error) {

	projectID := ""
	if auth != nil && auth.Metadata != nil {
		if pid, ok := auth.Metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
	}
	if projectID == "" {
		projectID = generateProjectID()
	}

	wrappedPayload := geminiToAntigravity(baseModel, translated, projectID)

	chatReq, errConvert := jsonToGenerateChatRequest(baseModel, wrappedPayload, projectID, auth)
	if errConvert != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("antigravity executor: convert to GenerateChatRequest: %w", errConvert)
	}

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	ua := resolveUserAgent(auth)

	for _, baseURL := range baseURLs {
		target := grpcTargetFromBaseURL(baseURL)

		client, _, errConn := grpcPool.getOrCreate(target, token, ua)
		if errConn != nil {
			log.Debugf("antigravity executor: gRPC dial error for %s: %v", target, errConn)
			continue
		}

		grpcCtx := grpcOutgoingMetadata(ctx, token, ua)
		chatResp, errCall := client.GenerateChat(grpcCtx, chatReq)
		if errCall != nil {
			log.Debugf("antigravity executor: gRPC GenerateChat error for %s: %v", target, errCall)
			continue
		}

		jsonPayload, errJSON := generateChatResponseToJSON(chatResp)
		if errJSON != nil {
			log.Debugf("antigravity executor: gRPC GenerateChat response to JSON error: %v", errJSON)
			continue
		}

		// Check if gRPC returned an empty/useless response — fall back to REST.
		if chatResp.Markdown == "" && len(chatResp.FunctionCalls) == 0 {
			log.Debugf("antigravity executor: gRPC GenerateChat returned empty response from %s, trying next", target)
			continue
		}

		appendAPIResponseChunk(ctx, e.cfg, jsonPayload)
		reporter.publish(ctx, parseAntigravityUsage(jsonPayload))

		var param any
		converted := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, jsonPayload, &param)
		reporter.ensurePublished(ctx)

		if len(converted) == 0 {
			log.Debugf("antigravity executor: gRPC GenerateChat translated to empty payload from %s, trying next", target)
			continue
		}
		log.Debugf("antigravity executor: using gRPC GenerateChat via %s (payload=%d bytes)", target, len(converted))
		sendTelemetryAfterChat(ctx, auth, token, projectID, ua, 0)
		return cliproxyexecutor.Response{Payload: []byte(converted)}, nil
	}

	return cliproxyexecutor.Response{}, fmt.Errorf("antigravity executor: all gRPC endpoints failed")
}

// executeStreamGRPC performs streaming via gRPC StreamGenerateChat.
func (e *AntigravityExecutor) executeStreamGRPC(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
	baseModel, token string, translated []byte, reporter *usageReporter, from, to sdktranslator.Format) (*cliproxyexecutor.StreamResult, error) {

	// Extract project ID
	projectID := ""
	if auth != nil && auth.Metadata != nil {
		if pid, ok := auth.Metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
	}
	if projectID == "" {
		projectID = generateProjectID()
	}

	// Wrap translated payload with antigravity fields (model, project, requestId, etc.)
	wrappedPayload := geminiToAntigravity(baseModel, translated, projectID)
	log.Debugf("antigravity executor: wrappedPayload (stream): %s", string(wrappedPayload))

	// Convert JSON to GenerateChatRequest protobuf
	chatReq, errConvert := jsonToGenerateChatRequest(baseModel, wrappedPayload, projectID, auth)
	if errConvert != nil {
		return nil, fmt.Errorf("antigravity executor: convert to GenerateChatRequest: %w", errConvert)
	}

	log.Debugf("antigravity executor: StreamGenerateChat request: project=%q request_id=%q model_config_id=%v user_message=%q history_len=%d func_decls=%d metadata=%+v",
		chatReq.Project, chatReq.RequestId, chatReq.ModelConfigId, chatReq.UserMessage, len(chatReq.History), len(chatReq.FunctionDeclarations), chatReq.Metadata)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	ua := resolveUserAgent(auth)

	for _, baseURL := range baseURLs {
		target := grpcTargetFromBaseURL(baseURL)

		client, _, errConn := grpcPool.getOrCreate(target, token, ua)
		if errConn != nil {
			log.Debugf("antigravity executor: gRPC dial error for %s: %v", target, errConn)
			continue
		}

		grpcCtx := grpcOutgoingMetadata(ctx, token, ua)
		stream, errStream := client.StreamGenerateChat(grpcCtx, chatReq)
		if errStream != nil {
			log.Debugf("antigravity executor: gRPC StreamGenerateChat error for %s: %v", target, errStream)
			continue
		}

		// Read the first chunk synchronously to detect immediate errors
		// (e.g. InvalidArgument) before committing to gRPC — enables REST fallback.
		firstResp, firstErr := stream.Recv()
		if firstErr != nil {
			if st, ok := status.FromError(firstErr); ok {
				log.Debugf("antigravity executor: gRPC first recv error for %s: code=%s msg=%s", target, st.Code(), st.Message())
			} else {
				log.Debugf("antigravity executor: gRPC first recv error for %s: %v", target, firstErr)
			}
			continue
		}

		out := make(chan cliproxyexecutor.StreamChunk)
		streamStartedAt := time.Now()
		go func() {
			defer close(out)
			var param any

			// Process the first response that was already received
			if jsonPayload, errJSON := generateChatResponseToJSON(firstResp); errJSON == nil {
				appendAPIResponseChunk(ctx, e.cfg, jsonPayload)
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(jsonPayload), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}

			for {
				resp, errRecv := stream.Recv()
				if errRecv == io.EOF {
					break
				}
				if errRecv != nil {
					if errors.Is(errRecv, context.Canceled) || errors.Is(errRecv, context.DeadlineExceeded) {
						out <- cliproxyexecutor.StreamChunk{Err: errRecv}
						return
					}
					if st, ok := status.FromError(errRecv); ok {
						log.Debugf("antigravity executor: gRPC recv error: code=%s msg=%s", st.Code(), st.Message())
					}
					recordAPIResponseError(ctx, e.cfg, errRecv)
					reporter.publishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: errRecv}
					return
				}

				jsonPayload, errJSON := generateChatResponseToJSON(resp)
				if errJSON != nil {
					log.Debugf("antigravity executor: gRPC response to JSON error: %v", errJSON)
					continue
				}

				appendAPIResponseChunk(ctx, e.cfg, jsonPayload)

				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(jsonPayload), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}
			// Send [DONE] signal
			var param2 any
			tail := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, []byte("[DONE]"), &param2)
			for i := range tail {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(tail[i])}
			}
			reporter.ensurePublished(ctx)
			sendTelemetryAfterChat(ctx, auth, token, projectID, ua, time.Since(streamStartedAt))
		}()

		log.Debugf("antigravity executor: using gRPC StreamGenerateChat via %s", target)
		return &cliproxyexecutor.StreamResult{Chunks: out}, nil
	}

	return nil, fmt.Errorf("antigravity executor: all gRPC endpoints failed")
}
