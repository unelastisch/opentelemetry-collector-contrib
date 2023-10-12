// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package githubactionseventreceiver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"
)

var errMissingEndpoint = errors.New("missing a receiver endpoint")

type githubActionsEventReceiver struct {
	nextConsumer    consumer.Traces
	config          *Config
	server          *http.Server
	shutdownWG      sync.WaitGroup
	createSettings  receiver.CreateSettings
	logger          *zap.Logger
	jsonUnmarshaler *jsonTracesUnmarshaler
	obsrecv         *receiverhelper.ObsReport
}

type jsonTracesUnmarshaler struct {
	logger *zap.Logger
}

func (j *jsonTracesUnmarshaler) UnmarshalTraces(blob []byte, config *Config) (ptrace.Traces, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(blob, &event); err != nil {
		j.logger.Error("Failed to unmarshal blob", zap.Error(err))
		return ptrace.Traces{}, err
	}

	var traces ptrace.Traces
	if _, ok := event["workflow_job"]; ok {
		var jobEvent WorkflowJobEvent
		err := json.Unmarshal(blob, &jobEvent)
		if err != nil {
			j.logger.Error("Failed to unmarshal job event", zap.Error(err))
			return ptrace.Traces{}, err
		}
		j.logger.Info("Unmarshalling WorkflowJobEvent")
		traces, err = eventToTraces(&jobEvent, config, j.logger)
		if err != nil {
			j.logger.Error("Failed to convert event to traces", zap.Error(err))
			return ptrace.Traces{}, err
		}
	} else if _, ok := event["workflow_run"]; ok {
		var runEvent WorkflowRunEvent
		err := json.Unmarshal(blob, &runEvent)
		if err != nil {
			j.logger.Error("Failed to unmarshal run event", zap.Error(err))
			return ptrace.Traces{}, err
		}
		j.logger.Info("Unmarshalling WorkflowRunEvent")
		traces, err = eventToTraces(&runEvent, config, j.logger)
		if err != nil {
			j.logger.Error("Failed to convert event to traces", zap.Error(err))
			return ptrace.Traces{}, err
		}
	} else {
		j.logger.Warn("Unknown event type")
		return ptrace.Traces{}, fmt.Errorf("unknown event type")
	}

	return traces, nil
}

func eventToTraces(event interface{}, config *Config, logger *zap.Logger) (ptrace.Traces, error) {
	logger.Info("Determining event")
	traces := ptrace.NewTraces()
	resourceSpans := traces.ResourceSpans().AppendEmpty()
	scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()

	switch e := event.(type) {
	case *WorkflowJobEvent:
		logger.Info("Processing WorkflowJobEvent")
		jobResource := resourceSpans.Resource()
		createResourceAttributes(jobResource, e, config, logger)
		traceID, err := generateTraceID(e.WorkflowJob.RunID, e.WorkflowJob.RunAttempt)
		if err != nil {
			logger.Error("Failed to generate trace ID", zap.Error(err))
			return traces, fmt.Errorf("failed to generate trace ID")
		}
		if e.WorkflowJob.Status == "completed" {
			parentSpanID := createParentSpan(scopeSpans, e.WorkflowJob.Steps, e.WorkflowJob, traceID, logger)
			processSteps(scopeSpans, e.WorkflowJob.Steps, e.WorkflowJob, traceID, parentSpanID, logger)
		}
	case *WorkflowRunEvent:
		logger.Info("Processing WorkflowRunEvent")
		runResource := resourceSpans.Resource()
		traceID, err := generateTraceID(e.WorkflowRun.ID, e.WorkflowRun.RunAttempt)
		if err != nil {
			logger.Error("Failed to generate trace ID", zap.Error(err))
			return traces, fmt.Errorf("failed to generate trace ID")
		}
		if e.WorkflowRun.Status == "completed" {
			createResourceAttributes(runResource, e, config, logger)
			createRootSpan(resourceSpans, e, traceID, logger)
		}
	default:
		logger.Error("unknown event type, dropping payload")
		return ptrace.Traces{}, fmt.Errorf("unknown event type, dropping payload")
	}

	return traces, nil
}

func createParentSpan(scopeSpans ptrace.ScopeSpans, steps []Step, job WorkflowJob, traceID pcommon.TraceID, logger *zap.Logger) pcommon.SpanID {
	logger.Info("Creating parent span", zap.String("name", job.Name))
	span := scopeSpans.Spans().AppendEmpty()
	span.SetTraceID(traceID)

	parentSpanID, _ := generateParentSpanID(job.RunID, job.RunAttempt)
	span.SetParentSpanID(parentSpanID)

	jobSpanID, _ := generateJobSpanID(job.ID, job.RunAttempt, job.Name)
	span.SetSpanID(jobSpanID)

	span.SetName(job.Name)
	span.SetKind(ptrace.SpanKindServer)
	if len(steps) > 0 {
		setSpanTimes(span, steps[0].StartedAt, steps[len(steps)-1].CompletedAt)
	} else {
		logger.Warn("No steps found, defaulting to job times")
		setSpanTimes(span, job.CreatedAt, job.CompletedAt)
	}

	allSuccessful := true
	anyFailure := false
	for _, step := range steps {
		if step.Status != "completed" || step.Conclusion != "success" {
			allSuccessful = false
		}
		if step.Conclusion == "failure" {
			anyFailure = true
			break
		}
	}

	if anyFailure {
		span.Status().SetCode(ptrace.StatusCodeError)
	} else if allSuccessful {
		span.Status().SetCode(ptrace.StatusCodeOk)
	} else {
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	return span.SpanID()
}

func createResourceAttributes(resource pcommon.Resource, event interface{}, config *Config, logger *zap.Logger) {
	attrs := resource.Attributes()

	switch e := event.(type) {
	case *WorkflowJobEvent:
		serviceName := generateServiceName(config, e.Repository.FullName)

		attrs.PutStr("service.name", serviceName)

		attrs.PutStr("ci.system", "github")
		attrs.PutStr("ci.actor", e.Repository.Owner.Login)

		attrs.PutStr("ci.github.job", e.WorkflowJob.Name)
		attrs.PutInt("ci.github.run_id", e.WorkflowJob.RunID)
		attrs.PutInt("ci.github.run_attempt", int64(e.WorkflowJob.RunAttempt))
		attrs.PutStr("ci.github.runner.name", e.WorkflowJob.RunnerName)
		attrs.PutStr("ci.github.workflow", e.WorkflowJob.WorkflowName)

		attrs.PutStr("scm.system", "git")
		attrs.PutStr("scm.git.branch", e.WorkflowJob.HeadBranch)
		attrs.PutStr("scm.git.sha", e.WorkflowJob.HeadSha)
		attrs.PutStr("scm.git.repo", e.Repository.FullName)

	case *WorkflowRunEvent:
		serviceName := generateServiceName(config, e.Repository.FullName)

		attrs.PutStr("service.name", serviceName)

		attrs.PutStr("ci.system", "github")
		attrs.PutStr("ci.actor", e.WorkflowRun.Repository.Owner.Login)

		attrs.PutInt("ci.github.run_id", e.WorkflowRun.ID)
		attrs.PutInt("ci.github.run_attempt", int64(e.WorkflowRun.RunAttempt))
		attrs.PutStr("ci.github.workflow", e.WorkflowRun.Name)
		attrs.PutStr("ci.github.workflow_path", e.WorkflowRun.Path)

		attrs.PutStr("scm.system", "git")
		attrs.PutStr("scm.git.branch", e.WorkflowRun.HeadBranch)
		attrs.PutStr("scm.git.sha", e.WorkflowRun.HeadSha)
		attrs.PutStr("scm.git.repo", e.Repository.FullName)

	default:
		logger.Error("unknown event type")
	}
}

func createRootSpan(resourceSpans ptrace.ResourceSpans, event *WorkflowRunEvent, traceID pcommon.TraceID, logger *zap.Logger) (pcommon.SpanID, error) {
	logger.Info("Creating root parent span", zap.String("name", event.WorkflowRun.Name))
	scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()
	span := scopeSpans.Spans().AppendEmpty()

	rootSpanID, err := generateParentSpanID(event.WorkflowRun.ID, event.WorkflowRun.RunAttempt)
	if err != nil {
		logger.Error("Failed to generate root span ID", zap.Error(err))
		return pcommon.SpanID{}, err
	}

	span.SetTraceID(traceID)
	span.SetSpanID(rootSpanID)
	span.SetName(event.WorkflowRun.Name)
	span.SetKind(ptrace.SpanKindServer)
	setSpanTimes(span, event.WorkflowRun.RunStartedAt, event.WorkflowRun.UpdatedAt)

	switch event.WorkflowRun.Conclusion {
	case "success":
		span.Status().SetCode(ptrace.StatusCodeOk)
	case "failure":
		span.Status().SetCode(ptrace.StatusCodeError)
	default:
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	span.Status().SetMessage(event.WorkflowRun.Conclusion)

	return rootSpanID, nil
}

func createSpan(scopeSpans ptrace.ScopeSpans, step Step, traceID pcommon.TraceID, parentSpanID pcommon.SpanID, logger *zap.Logger) pcommon.SpanID {
	logger.Info("Processing span", zap.String("step_name", step.Name))
	span := scopeSpans.Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.SetParentSpanID(parentSpanID)
	span.SetSpanID(generateSpanID())
	setSpanTimes(span, step.StartedAt, step.CompletedAt)
	span.SetName(step.Name)
	span.SetKind(ptrace.SpanKindServer)

	switch step.Conclusion {
	case "success":
		span.Status().SetCode(ptrace.StatusCodeOk)
	case "failure":
		span.Status().SetCode(ptrace.StatusCodeError)
	default:
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	span.Status().SetMessage(step.Conclusion)

	return span.SpanID()
}

func generateTraceID(runID int64, runAttempt int) (pcommon.TraceID, error) {
	input := fmt.Sprintf("%d%dt", runID, runAttempt)
	hash := sha256.Sum256([]byte(input))
	traceIDHex := hex.EncodeToString(hash[:])

	var traceID pcommon.TraceID
	_, err := hex.Decode(traceID[:], []byte(traceIDHex[:32]))
	if err != nil {
		return pcommon.TraceID{}, err
	}

	return traceID, nil
}

func generateJobSpanID(runID int64, runAttempt int, job string) (pcommon.SpanID, error) {
	input := fmt.Sprintf("%d%d%s", runID, runAttempt, job)
	hash := sha256.Sum256([]byte(input))
	spanIDHex := hex.EncodeToString(hash[:])

	var spanID pcommon.SpanID
	_, err := hex.Decode(spanID[:], []byte(spanIDHex[16:32]))
	if err != nil {
		return pcommon.SpanID{}, err
	}

	return spanID, nil
}

func generateParentSpanID(runID int64, runAttempt int) (pcommon.SpanID, error) {
	input := fmt.Sprintf("%d%ds", runID, runAttempt)
	hash := sha256.Sum256([]byte(input))
	spanIDHex := hex.EncodeToString(hash[:])

	var spanID pcommon.SpanID
	_, err := hex.Decode(spanID[:], []byte(spanIDHex[16:32]))
	if err != nil {
		return pcommon.SpanID{}, err
	}

	return spanID, nil
}

func generateServiceName(config *Config, fullName string) string {
	if config.CustomServiceName != "" {
		return config.CustomServiceName
	}
	formattedName := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(fullName, "/", "-"), "_", "-"))
	return fmt.Sprintf("%s%s%s", config.ServiceNamePrefix, formattedName, config.ServiceNameSuffix)
}

func generateSpanID() pcommon.SpanID {
	var spanID pcommon.SpanID
	binary.Read(rand.Reader, binary.BigEndian, &spanID)
	return spanID
}

func processSteps(scopeSpans ptrace.ScopeSpans, steps []Step, job WorkflowJob, traceID pcommon.TraceID, parentSpanID pcommon.SpanID, logger *zap.Logger) {
	for _, step := range steps {
		createSpan(scopeSpans, step, traceID, parentSpanID, logger)
	}
}

func setSpanTimes(span ptrace.Span, start, end time.Time) {
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(end))
}

func validateSignatureSHA256(secret string, signatureHeader string, body []byte, logger *zap.Logger) bool {
	if signatureHeader == "" || len(signatureHeader) < 7 {
		logger.Debug("Unauthorized - No Signature Header")
		return false
	}
	receivedSig := signatureHeader[7:]
	computedHash := hmac.New(sha256.New, []byte(secret))
	computedHash.Write(body)
	expectedSig := hex.EncodeToString(computedHash.Sum(nil))

	logger.Info("Debugging Signatures", zap.String("Received", receivedSig), zap.String("Computed", expectedSig))

	return hmac.Equal([]byte(expectedSig), []byte(receivedSig))
}

func validateSignatureSHA1(secret string, signatureHeader string, body []byte, logger *zap.Logger) bool {
	if signatureHeader == "" {
		logger.Debug("Unauthorized - No Signature Header")
		return false
	}
	receivedSig := signatureHeader[5:] // Assume "sha1=" prefix
	computedHash := hmac.New(sha1.New, []byte(secret))
	computedHash.Write(body)
	expectedSig := hex.EncodeToString(computedHash.Sum(nil))

	logger.Info("Debugging Signatures", zap.String("Received", receivedSig), zap.String("Computed", expectedSig))

	return hmac.Equal([]byte(expectedSig), []byte(receivedSig))
}

func newTracesReceiver(
	params receiver.CreateSettings,
	config *Config,
	nextConsumer consumer.Traces,
) (*githubActionsEventReceiver, error) {
	if nextConsumer == nil {
		return nil, component.ErrNilNextConsumer
	}

	if config.Endpoint == "" {
		return nil, errMissingEndpoint
	}

	transport := "http"
	if config.TLSSetting != nil {
		transport = "https"
	}

	obsrecv, err := receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             params.ID,
		Transport:              transport,
		ReceiverCreateSettings: params,
	})

	if err != nil {
		return nil, err
	}

	gaer := &githubActionsEventReceiver{
		nextConsumer:   nextConsumer,
		config:         config,
		createSettings: params,
		logger:         params.Logger,
		jsonUnmarshaler: &jsonTracesUnmarshaler{
			logger: params.Logger,
		},
		obsrecv: obsrecv,
	}

	return gaer, nil
}

func (gaer *githubActionsEventReceiver) Start(ctx context.Context, host component.Host) error {
	endpint := fmt.Sprintf("%s%s", gaer.config.Endpoint, gaer.config.Path)
	gaer.logger.Info("Starting GithubActionsEvent server", zap.String("endpoint", endpint))
	gaer.server = &http.Server{
		Addr:    gaer.config.HTTPServerSettings.Endpoint,
		Handler: gaer,
	}

	gaer.shutdownWG.Add(1)
	go func() {
		defer gaer.shutdownWG.Done()
		if err := gaer.server.ListenAndServe(); err != http.ErrServerClosed {
			host.ReportFatalError(err)
		}
	}()

	return nil
}

func (gaer *githubActionsEventReceiver) Shutdown(ctx context.Context) error {
	var err error
	if gaer.server != nil {
		err = gaer.server.Close()
	}
	gaer.shutdownWG.Wait()
	return err
}

func (gaer *githubActionsEventReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userAgent := r.Header.Get("User-Agent")
	if !strings.HasPrefix(userAgent, "GitHub-Hookshot") {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if r.URL.Path != gaer.config.Path {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	defer r.Body.Close()

	slurp, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	// Validate the request if Secret is set in the configuration
	if gaer.config.Secret != "" {
		signatureSHA256 := r.Header.Get("X-Hub-Signature-256")
		if signatureSHA256 != "" && !validateSignatureSHA256(gaer.config.Secret, signatureSHA256, slurp, gaer.logger) {
			gaer.logger.Debug("Unauthorized - Signature Mismatch SHA256")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		} else {
			signatureSHA1 := r.Header.Get("X-Hub-Signature")
			if signatureSHA1 != "" && !validateSignatureSHA1(gaer.config.Secret, signatureSHA1, slurp, gaer.logger) {
				gaer.logger.Debug("Unauthorized - Signature Mismatch SHA1")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
	}

	gaer.logger.Debug("Received request", zap.ByteString("payload", slurp))

	td, err := gaer.jsonUnmarshaler.UnmarshalTraces(slurp, gaer.config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gaer.logger.Info("Unmarshaled spans", zap.Int("#spans", td.SpanCount()))

	// Pass the traces to the nextConsumer
	consumerErr := gaer.nextConsumer.ConsumeTraces(ctx, td)
	if consumerErr != nil {
		gaer.logger.Error("Failed to process traces", zap.Error(consumerErr))
		http.Error(w, "Failed to process traces", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
