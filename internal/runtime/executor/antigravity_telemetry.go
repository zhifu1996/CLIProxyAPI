package executor

import (
	"context"
	"math/rand"
	"sync"
	"time"

	pb "github.com/router-for-me/CLIProxyAPI/v6/internal/proto/v1internal"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// telemetryState tracks per-connection telemetry counters to make
// AiCharactersReport time_interval_index realistic across requests.
var telemetryState struct {
	mu                sync.Mutex
	intervalIndex     int64
	lastCharReportAt  time.Time
	charReportCounter int // count requests since last char report
}

// sendTelemetryAfterChat fires async telemetry calls that mimic a real
// Antigravity client after a successful chat response.
//
// It sends:
// 1. RecordCodeAssistMetrics with a ConversationOffered event (every request)
// 2. RecordClientEvent with ConversationInteraction (random ~30% chance)
// 3. RecordCodeAssistMetrics with AiCharactersReports (every ~5 requests)
func sendTelemetryAfterChat(ctx context.Context, auth *cliproxyauth.Auth, token, projectID, userAgent string, streamLatency time.Duration) {
	// Detach from request context to avoid cancellation killing telemetry.
	bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	go func() {
		defer cancel()
		sendTelemetrySync(bgCtx, auth, token, projectID, userAgent, streamLatency)
	}()
}

func sendTelemetrySync(ctx context.Context, auth *cliproxyauth.Auth, token, projectID, userAgent string, streamLatency time.Duration) {
	baseURLs := antigravityBaseURLFallbackOrder(auth)
	if len(baseURLs) == 0 {
		return
	}
	target := grpcTargetFromBaseURL(baseURLs[0])

	client, _, err := grpcPool.getOrCreate(target, token, userAgent)
	if err != nil {
		log.Debugf("antigravity telemetry: gRPC dial error: %v", err)
		return
	}

	grpcCtx := grpcOutgoingMetadata(ctx, token, userAgent)
	now := timestamppb.Now()

	// 1) ConversationOffered — always
	sendConversationOffered(grpcCtx, client, auth, projectID, now, streamLatency)

	// 2) ConversationInteraction — ~30% chance
	if telemetryRand(100) < 30 {
		sendConversationInteraction(grpcCtx, client, projectID, now)
	}

	// 3) AiCharactersReports — every ~5 requests or every 2+ minutes
	telemetryState.mu.Lock()
	telemetryState.charReportCounter++
	shouldSendChars := telemetryState.charReportCounter >= 5 ||
		time.Since(telemetryState.lastCharReportAt) > 2*time.Minute
	if shouldSendChars {
		telemetryState.charReportCounter = 0
		telemetryState.lastCharReportAt = time.Now()
		telemetryState.intervalIndex++
	}
	idx := telemetryState.intervalIndex
	telemetryState.mu.Unlock()

	if shouldSendChars {
		sendAiCharactersReports(grpcCtx, client, auth, projectID, now, idx)
	}
}

func sendConversationOffered(ctx context.Context, client pb.CloudCodeClient, auth *cliproxyauth.Auth, projectID string, ts *timestamppb.Timestamp, latency time.Duration) {
	firstMsg := latency
	if firstMsg <= 0 {
		firstMsg = time.Duration(800+telemetryRand(1200)) * time.Millisecond
	}

	req := &pb.RecordCodeAssistMetricsRequest{
		Project:  projectID,
		Metadata: buildClientMetadata(auth),
		Metrics: []*pb.CodeAssistMetric{
			{
				Timestamp: ts,
				Event: &pb.CodeAssistMetric_ConversationOffered{
					ConversationOffered: &pb.ConversationOffered{
						Status:           pb.ActionStatus_ACTION_STATUS_NO_ERROR,
						IsAgentic:        true,
						InitiationMethod: pb.ConversationOffered_AGENT,
						StreamingLatency: &pb.ConversationOffered_StreamingLatency{
							FirstMessageLatency: durationpb.New(firstMsg),
							TotalLatency:        durationpb.New(latency),
						},
					},
				},
			},
		},
	}

	if _, err := client.RecordCodeAssistMetrics(ctx, req); err != nil {
		log.Debugf("antigravity telemetry: RecordCodeAssistMetrics (ConversationOffered) error: %v", err)
	}
}

func sendConversationInteraction(ctx context.Context, client pb.CloudCodeClient, projectID string, ts *timestamppb.Timestamp) {
	interactions := []pb.ConversationInteraction_Interaction{
		pb.ConversationInteraction_COPY,
		pb.ConversationInteraction_INSERT,
		pb.ConversationInteraction_ACCEPT_CODE_BLOCK,
		pb.ConversationInteraction_ACCEPT_ALL,
	}
	chosen := interactions[telemetryRand(len(interactions))]

	var acceptedLines *int64
	if chosen == pb.ConversationInteraction_INSERT || chosen == pb.ConversationInteraction_ACCEPT_CODE_BLOCK ||
		chosen == pb.ConversationInteraction_ACCEPT_ALL {
		v := int64(3 + telemetryRand(50))
		acceptedLines = &v
	}

	req := &pb.RecordClientEventRequest{
		Project: projectID,
		IdeType: pb.ClientMetadata_ANTIGRAVITY,
		Metric: &pb.CodeAssistMetric{
			Timestamp: ts,
			Event: &pb.CodeAssistMetric_ConversationInteraction{
				ConversationInteraction: &pb.ConversationInteraction{
					Status:           pb.ActionStatus_ACTION_STATUS_NO_ERROR,
					Interaction:      chosen,
					AcceptedLines:    acceptedLines,
					IsAgentic:        true,
					InitiationMethod: pb.ConversationOffered_AGENT,
				},
			},
		},
	}

	if _, err := client.RecordClientEvent(ctx, req); err != nil {
		log.Debugf("antigravity telemetry: RecordClientEvent (ConversationInteraction) error: %v", err)
	}
}

func sendAiCharactersReports(ctx context.Context, client pb.CloudCodeClient, auth *cliproxyauth.Auth, projectID string, ts *timestamppb.Timestamp, intervalIdx int64) {
	totalChars := int64(200 + telemetryRand(2000))
	wsChars := totalChars / int64(4+telemetryRand(3))

	req := &pb.RecordCodeAssistMetricsRequest{
		Project:  projectID,
		Metadata: buildClientMetadata(auth),
		Metrics: []*pb.CodeAssistMetric{
			{
				Timestamp: ts,
				Event: &pb.CodeAssistMetric_AiCharactersReports{
					AiCharactersReports: &pb.AiCharactersReports{
						Reports: []*pb.AiCharactersReport{
							{
								Language:          "go",
								EditType:          pb.AiCharactersReport_AI_GENERATION,
								TotalChars:        totalChars,
								WhitespaceChars:   wsChars,
								TimeIntervalIndex: intervalIdx,
							},
						},
					},
				},
			},
		},
	}

	if _, err := client.RecordCodeAssistMetrics(ctx, req); err != nil {
		log.Debugf("antigravity telemetry: RecordCodeAssistMetrics (AiCharactersReports) error: %v", err)
	}
}

// telemetryRand returns a random int in [0, n) using the shared rand source.
func telemetryRand(n int) int {
	if n <= 0 {
		return 0
	}
	randSourceMutex.Lock()
	v := randSource.Intn(n)
	randSourceMutex.Unlock()
	return v
}

func init() {
	telemetryState.lastCharReportAt = time.Now()
	// Seed with small random noise so the first char report doesn't fire immediately.
	telemetryState.charReportCounter = rand.Intn(3)
}
