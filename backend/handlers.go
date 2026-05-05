package handlers

import (
	appMiddleware "backend/middleware"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer is used to create spans for this package.
var tracer = otel.Tracer("handlers")

// Server holds dependencies for API handlers.
type Server struct {
	Auth               appMiddleware.TokenVerifier
	CB                 *gobreaker.CircuitBreaker
	Client             *http.Client
	ExternalServiceURL string
}

// ErrorResponse provides a standardized JSON structure for all API errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// SendError is a helper to transmit structured JSON errors with the appropriate status code.
func SendError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{Error: message})
}

func (s *Server) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func (s *Server) ReadyCheck(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		SendError(w, "Firebase Auth client not initialized", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Ready")
}

func (s *Server) RootHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve the span created by the otelchi middleware
	span := trace.SpanFromContext(r.Context())

	// Add custom business attributes to the trace
	span.SetAttributes(attribute.String("app.version", "v1.0.0"), attribute.Bool("app.control_plane", true))

	// Implementation of Suggestion 1: Using a decorator pattern to track internal operations
	_ = s.WithTrace(r.Context(), "contract.logic.processing", func(ctx context.Context) error {
		// This block is now its own span in the trace
		time.Sleep(40 * time.Millisecond) // Mock business logic
		return nil
	})

	// Implementation of Suggestion 2: Propagating trace context to an external service
	s.CallExternalService(r.Context())

	fmt.Fprintf(w, "Contract Record Keeper: Control Plane Active")
}

// WithTrace is a decorator-style helper that wraps a function call in its own OpenTelemetry span.
// This tracks duration and automatically records errors if the function fails.
func (s *Server) WithTrace(ctx context.Context, spanName string, fn func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, spanName)
	defer span.End()

	err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// CallExternalService demonstrates outgoing trace propagation using the otelhttp package.
func (s *Server) CallExternalService(ctx context.Context) {
	// Wrap the call in a circuit breaker to prevent cascading failures
	_, err := s.CB.Execute(func() (interface{}, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", s.ExternalServiceURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		resp, err := s.Client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		// Circuit breaker trips if we get consistent 5xx errors
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("external service error: %d", resp.StatusCode)
		}

		return nil, nil
	})

	if err != nil {
		slog.ErrorContext(ctx, "external service call failed", "error", err)
	}
}

func NotFoundHandler(w http.ResponseWriter, r *http.Request) {
	SendError(w, "The requested resource was not found", http.StatusNotFound)
}
