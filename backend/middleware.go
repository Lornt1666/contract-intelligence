package middleware

import (
	"backend/handlers"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"firebase.google.com/go/v4/auth"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type TokenVerifier interface {
	VerifyIDToken(ctx context.Context, idToken string) (*auth.Token, error)
}

type ctxKey int

const (
	ContextKeyUser ctxKey = iota
	ContextKeyCorrelationID
)

var (
	authAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auth_attempts_total",
		Help: "Total number of Firebase authentication attempts.",
	}, []string{"result"}) // "success" or "failure"
)

// ContextHandler wraps a slog.Handler to automatically inject context values.
type ContextHandler struct {
	slog.Handler
}

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := ctx.Value(ContextKeyCorrelationID).(string); ok {
		r.AddAttrs(slog.String("correlation_id", id))
	}

	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		r.AddAttrs(slog.String("trace_id", spanContext.TraceID().String()))
		r.AddAttrs(slog.String("span_id", spanContext.SpanID().String()))
	}

	return h.Handler.Handle(ctx, r)
}

func NewContextHandler(h slog.Handler) *ContextHandler {
	return &ContextHandler{h}
}

func VerifyFirebaseToken(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			correlationID, _ := r.Context().Value(ContextKeyCorrelationID).(string)
			span := trace.SpanFromContext(r.Context())

			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				msg := "Unauthorized: Missing Bearer Token"
				slog.Warn(msg, "correlation_id", correlationID)
				span.RecordError(fmt.Errorf(msg))
				span.SetStatus(codes.Error, msg)
				authAttempts.WithLabelValues("failure").Inc()
				handlers.SendError(w, msg, http.StatusUnauthorized)
				return
			}

			idToken := strings.TrimPrefix(authHeader, "Bearer ")
			token, err := verifier.VerifyIDToken(r.Context(), idToken)
			if err != nil {
				slog.Error("Unauthorized: Token verification failed", "error", err, "correlation_id", correlationID)
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				authAttempts.WithLabelValues("failure").Inc()
				handlers.SendError(w, fmt.Sprintf("Unauthorized: %v", err), http.StatusUnauthorized)
				return
			}

			authAttempts.WithLabelValues("success").Inc()
			ctx := context.WithValue(r.Context(), ContextKeyUser, token.UID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Check if the origin is in our whitelist
			isAllowed := false
			for _, o := range allowedOrigins {
				if o == "*" || o == origin {
					isAllowed = true
					break
				}
			}

			if isAllowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Correlation-ID")
				w.Header().Add("Vary", "Origin")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func getCorrelationID(r *http.Request) string {
	id := r.Header.Get("X-Correlation-ID")
	if id == "" {
		id = r.Header.Get("X-Cloud-Trace-Context")
	}
	if id == "" {
		newID, err := uuid.NewRandom()
		if err != nil {
			return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
		}
		id = newID.String()
	}
	return id
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusResponseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func JSONLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		correlationID := getCorrelationID(r)

		rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}

		w.Header().Set("X-Correlation-ID", correlationID)
		ctx := context.WithValue(r.Context(), ContextKeyCorrelationID, correlationID)

		next.ServeHTTP(rw, r.WithContext(ctx))

		duration := time.Since(start)

		slog.Info("Request handled",
			"severity", "INFO",
			"httpRequest", map[string]interface{}{
				"requestMethod": r.Method,
				"requestUrl":    r.URL.String(),
				"status":        rw.status,
				"latency":       fmt.Sprintf("%.9fs", duration.Seconds()),
				"remoteIp":      r.RemoteAddr,
				"userAgent":     r.UserAgent(),
			},
		)
	})
}
