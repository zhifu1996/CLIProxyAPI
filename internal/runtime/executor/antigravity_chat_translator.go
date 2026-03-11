package executor

import (
	"encoding/json"
	"runtime"
	"strings"

	aiplatformpb "github.com/router-for-me/CLIProxyAPI/v6/internal/proto/aiplatform"
	pb "github.com/router-for-me/CLIProxyAPI/v6/internal/proto/v1internal"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/types/known/structpb"
)

// jsonToGenerateChatRequest converts a translated Gemini JSON payload into a GenerateChatRequest protobuf.
func jsonToGenerateChatRequest(model string, payload []byte, projectID string, auth *cliproxyauth.Auth) (*pb.GenerateChatRequest, error) {
	root := gjson.ParseBytes(payload)

	req := &pb.GenerateChatRequest{
		Project:        projectID,
		RequestId:      root.Get("requestId").String(),
		ModelConfigId:  ptrString(model),
		UserPromptId:   ptrString(root.Get("request.userPromptId").String()),
		Metadata:       buildClientMetadata(auth),
		EnablePromptEnhancement: false,
	}

	// Extract function declarations from request.tools
	toolsResult := root.Get("request.tools")
	if toolsResult.IsArray() {
		for _, tool := range toolsResult.Array() {
			fdResult := tool.Get("functionDeclarations")
			if !fdResult.IsArray() {
				continue
			}
			for _, fd := range fdResult.Array() {
				funcDecl := &aiplatformpb.FunctionDeclaration{
					Name:        fd.Get("name").String(),
					Description: fd.Get("description").String(),
				}
				if params := fd.Get("parameters"); params.Exists() {
					funcDecl.Parameters = jsonToStruct(params.Raw)
				}
				req.FunctionDeclarations = append(req.FunctionDeclarations, funcDecl)
			}
		}
	}

	// Build history and user_message from request.contents
	contentsResult := root.Get("request.contents")
	if contentsResult.IsArray() {
		contents := contentsResult.Array()
		for i, content := range contents {
			role := content.Get("role").String()
			isLast := i == len(contents)-1

			// Last user message becomes user_message
			if isLast && role == "user" {
				req.UserMessage = extractTextFromParts(content.Get("parts"))
				continue
			}

			msg := contentToChatMessage(content)
			if msg != nil {
				req.History = append(req.History, msg)
			}
		}
	}

	// If no user message was extracted from contents, use top-level user_message if present
	if req.UserMessage == "" {
		if um := root.Get("userMessage"); um.Exists() {
			req.UserMessage = um.String()
		}
	}

	// System instruction → prepend as SYSTEM ChatMessage in history
	sysInstruction := root.Get("request.systemInstruction")
	if sysInstruction.Exists() {
		sysText := extractTextFromParts(sysInstruction.Get("parts"))
		if sysText != "" {
			sysMsg := &pb.ChatMessage{
				Author:  ptrChatMessageEntityType(pb.ChatMessage_SYSTEM),
				Content: ptrString(sysText),
			}
			req.History = append([]*pb.ChatMessage{sysMsg}, req.History...)
		}
	}

	return req, nil
}

// generateChatResponseToJSON converts a GenerateChatResponse protobuf into Gemini-compatible JSON.
func generateChatResponseToJSON(resp *pb.GenerateChatResponse) ([]byte, error) {
	result := make(map[string]interface{})

	// Build candidate
	candidate := map[string]interface{}{
		"content": map[string]interface{}{
			"role":  "model",
			"parts": chatResponseToParts(resp),
		},
	}

	// Map finish_reason
	if resp.FinishReason != pb.FinishReason_FINISH_REASON_UNSPECIFIED {
		candidate["finishReason"] = mapFinishReason(resp.FinishReason)
	}

	result["candidates"] = []interface{}{candidate}

	// Extract trace_id from processing details
	if resp.ProcessingDetails != nil {
		if resp.ProcessingDetails.TraceId != nil {
			result["traceId"] = *resp.ProcessingDetails.TraceId
		}
		if resp.ProcessingDetails.ModelConfig != nil {
			result["modelVersion"] = resp.ProcessingDetails.ModelConfig.Id
		}
	}

	return json.Marshal(result)
}

func chatResponseToParts(resp *pb.GenerateChatResponse) []interface{} {
	var parts []interface{}

	// Text content
	if resp.Markdown != "" {
		parts = append(parts, map[string]interface{}{
			"text": resp.Markdown,
		})
	}

	// Function calls
	for _, fc := range resp.FunctionCalls {
		fcMap := map[string]interface{}{
			"functionCall": map[string]interface{}{
				"name": fc.Name,
				"args": structToMap(fc.Args),
			},
		}
		parts = append(parts, fcMap)
	}

	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{
			"text": "",
		})
	}

	return parts
}

func mapFinishReason(fr pb.FinishReason) string {
	switch fr {
	case pb.FinishReason_FINISH_REASON_STOP:
		return "STOP"
	case pb.FinishReason_FINISH_REASON_RECITATION:
		return "RECITATION"
	case pb.FinishReason_FINISH_REASON_PROHIBITED_CONTENT:
		return "SAFETY"
	default:
		return "STOP"
	}
}

func contentToChatMessage(content gjson.Result) *pb.ChatMessage {
	role := content.Get("role").String()
	parts := content.Get("parts")

	msg := &pb.ChatMessage{}

	switch role {
	case "user":
		msg.Author = ptrChatMessageEntityType(pb.ChatMessage_USER)
	case "model":
		msg.Author = ptrChatMessageEntityType(pb.ChatMessage_SYSTEM)
	default:
		msg.Author = ptrChatMessageEntityType(pb.ChatMessage_UNKNOWN)
	}

	if !parts.IsArray() {
		return msg
	}

	// Check for function call/response in parts
	for _, part := range parts.Array() {
		if fc := part.Get("functionCall"); fc.Exists() {
			msg.FunctionCall = &aiplatformpb.FunctionCall{
				Name: fc.Get("name").String(),
				Args: jsonToStruct(fc.Get("args").Raw),
			}
			return msg
		}
		if fr := part.Get("functionResponse"); fr.Exists() {
			msg.FunctionResponse = &aiplatformpb.FunctionResponse{
				Name:     fr.Get("name").String(),
				Response: jsonToStruct(fr.Get("response").Raw),
			}
			return msg
		}
		// Handle inline_data / blob
		if inlineData := part.Get("inlineData"); inlineData.Exists() {
			msg.Blob = &pb.ChatMessage_Blob{
				MimeType: inlineData.Get("mimeType").String(),
				Data:     []byte(inlineData.Get("data").String()),
			}
			return msg
		}
	}

	// Plain text content
	text := extractTextFromParts(parts)
	if text != "" {
		msg.Content = ptrString(text)
	}

	return msg
}

func extractTextFromParts(parts gjson.Result) string {
	if !parts.IsArray() {
		return ""
	}
	var sb strings.Builder
	for _, part := range parts.Array() {
		if text := part.Get("text"); text.Exists() {
			sb.WriteString(text.String())
		}
	}
	return sb.String()
}

func buildClientMetadata(auth *cliproxyauth.Auth) *pb.ClientMetadata {
	md := &pb.ClientMetadata{
		IdeType:    pb.ClientMetadata_ANTIGRAVITY,
		IdeVersion: resolveAntigravityVersion(auth),
		Platform:   resolvePlatformEnum(),
		PluginType: pb.ClientMetadata_GEMINI,
	}
	return md
}

func resolveAntigravityVersion(auth *cliproxyauth.Auth) string {
	ua := resolveUserAgent(auth)
	// Parse version from "antigravity/1.19.6 ..." format
	parts := strings.SplitN(ua, "/", 2)
	if len(parts) >= 2 {
		version := strings.SplitN(parts[1], " ", 2)
		if len(version) >= 1 {
			return version[0]
		}
	}
	return "1.19.6"
}

func resolvePlatformEnum() pb.ClientMetadata_Platform {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/amd64":
		return pb.ClientMetadata_DARWIN_AMD64
	case "darwin/arm64":
		return pb.ClientMetadata_DARWIN_ARM64
	case "linux/amd64":
		return pb.ClientMetadata_LINUX_AMD64
	case "linux/arm64":
		return pb.ClientMetadata_LINUX_ARM64
	case "windows/amd64":
		return pb.ClientMetadata_WINDOWS_AMD64
	default:
		return pb.ClientMetadata_PLATFORM_UNSPECIFIED
	}
}

// Helper functions

func ptrString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrChatMessageEntityType(t pb.ChatMessage_EntityType) *pb.ChatMessage_EntityType {
	return &t
}

func jsonToStruct(raw string) *structpb.Struct {
	if raw == "" || raw == "null" {
		return nil
	}
	s := &structpb.Struct{}
	if err := s.UnmarshalJSON([]byte(raw)); err != nil {
		return nil
	}
	return s
}

func structToMap(s *structpb.Struct) map[string]interface{} {
	if s == nil {
		return map[string]interface{}{}
	}
	return s.AsMap()
}
