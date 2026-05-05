package main

import (
	"backend/handlers"
	appMiddleware "backend/middleware"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sony/gobreaker"
	"firebase.google.com/go/v4/auth"
	"go.opentelemetry.io/otel/trace"
)

type mockAuthClient struct {
	verifyFunc func(ctx context.Context, idToken string) (*auth.Token, error)
}

func (m *mockAuthClient) VerifyIDToken(ctx context.Context, idToken string) (*auth.Token, error) {
	if m.verifyFunc == nil {
		return nil, fmt.Errorf("mock error")
	}
	return m.verifyFunc(ctx, idToken)
}

func TestVerifyFirebaseToken(t *testing.T) {
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name           string
		authHeader     string
		mockVerify     func(ctx context.Context, idToken string) (*auth.Token, error)
		expectedStatus int
	}{
		{
			name:           "Missing Header",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:       "Valid Token",
			authHeader: "Bearer valid-token",
			mockVerify: func(ctx context.Context, idToken string) (*auth.Token, error) {
				return &auth.Token{UID: "user-123"}, nil
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:       "Invalid Token",
			authHeader: "Bearer invalid-token",
			mockVerify: func(ctx context.Context, idToken string) (*auth.Token, error) {
				return nil, fmt.Errorf("invalid token")
			},
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mAuth := &mockAuthClient{verifyFunc: tt.mockVerify}
			req := httptest.NewRequest("GET", "/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rr := httptest.NewRecorder()

			mw := appMiddleware.VerifyFirebaseToken(mAuth)
			appMiddleware.JSONLoggingMiddleware(mw(nextHandler)).ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %v, got %v", tt.expectedStatus, rr.Code)
			}
		})
	}
}

func TestJSONLoggingMiddleware(t *testing.T) {
	t.Run("Generates New ID", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		rr := httptest.NewRecorder()

		handler := appMiddleware.JSONLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := r.Context().Value(appMiddleware.ContextKeyCorrelationID).(string)
			if !ok || id == "" {
				t.Error("correlation_id not found in context")
			}
		}))

		handler.ServeHTTP(rr, req)

		if rr.Header().Get("X-Correlation-ID") == "" {
			t.Error("X-Correlation-ID header not set in response")
		}
	})

	t.Run("Propagates Existing ID", func(t *testing.T) {
		existingID := "test-id-123"
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Correlation-ID", existingID)
		rr := httptest.NewRecorder()

		handler := appMiddleware.JSONLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Context().Value(appMiddleware.ContextKeyCorrelationID).(string)
			if id != existingID {
				t.Errorf("expected correlation_id %s, got %s", existingID, id)
			}
		}))

		handler.ServeHTTP(rr, req)

		if rr.Header().Get("X-Correlation-ID") != existingID {
			t.Errorf("expected X-Correlation-ID header %s, got %s", existingID, rr.Header().Get("X-Correlation-ID"))
		}
	})
}

func TestRootHandler(t *testing.T) {
	// 1. Setup mock external service to avoid real network calls
	mockExternal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockExternal.Close()

	// 2. Initialize Server with mock dependencies
	h := &handlers.Server{
		CB:                 gobreaker.NewCircuitBreaker(gobreaker.Settings{}),
		Client:             mockExternal.Client(),
		ExternalServiceURL: mockExternal.URL,
	}

	// 3. Create request and response recorder
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	// 4. Execute handler
	h.RootHandler(rr, req)

	// 5. Assert results
	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v", rr.Code)
	}
	expected := "Contract Record Keeper: Control Plane Active"
	if rr.Body.String() != expected {
		t.Errorf("expected body %q, got %q", expected, rr.Body.String())
	}
}

func TestContextHandler_Enrichment(t *testing.T) {
	var buf bytes.Buffer
	// Setup handler with JSON output to a buffer
	h := appMiddleware.NewContextHandler(slog.NewJSONHandler(&buf, nil))
	logger := slog.New(h)

	// 1. Create a dummy trace context
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	// 2. Add correlation ID
	ctx = context.WithValue(ctx, appMiddleware.ContextKeyCorrelationID, "test-correlation-id")

	// 3. Log something
	logger.InfoContext(ctx, "hello world")

	// 4. Verify JSON output
	var result map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("Failed to parse log output: %v", err)
	}

	if result["trace_id"] != traceID.String() {
		t.Errorf("Expected trace_id %s, got %v", traceID.String(), result["trace_id"])
	}
	if result["span_id"] != spanID.String() {
		t.Errorf("Expected span_id %s, got %v", spanID.String(), result["span_id"])
	}
	if result["correlation_id"] != "test-correlation-id" {
		t.Errorf("Expected correlation_id test-correlation-id, got %v", result["correlation_id"])
	}
}

func TestNotFoundHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/not-found", nil)
	rr := httptest.NewRecorder()

	handlers.NotFoundHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %v", rr.Code)
	}

	var resp handlers.ErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != "The requested resource was not found" {
		t.Errorf("expected error message 'The requested resource was not found', got '%v'", resp.Error)
	}

	if contentType := rr.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %v", contentType)
	}
}

func TestReadyCheck(t *testing.T) {
	t.Run("Ready when Auth is present", func(t *testing.T) {
		h := &handlers.Server{
			Auth: &mockAuthClient{},
			CB:   gobreaker.NewCircuitBreaker(gobreaker.Settings{}),
		}
		req := httptest.NewRequest("GET", "/ready", nil)
		rr := httptest.NewRecorder()
		h.ReadyCheck(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("Not Ready when Auth is missing", func(t *testing.T) {
		h := &handlers.Server{Auth: nil}
		req := httptest.NewRequest("GET", "/ready", nil)
		rr := httptest.NewRecorder()
		h.ReadyCheck(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503, got %d", rr.Code)
		}
	})
}
