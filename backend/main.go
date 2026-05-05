package main

import (
	"backend/handlers"
	appMiddleware "backend/middleware"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	firebase "firebase.google.com/go/v4"
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	_ "github.com/lib/pq" // Example Postgres driver
)

type Config struct {
	Port               string
	ProjectID          string
	AllowedOrigins     []string
	ExternalServiceURL string
	DatabaseURL        string
}

func loadConfig() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	origins := os.Getenv("ALLOWED_ORIGINS")
	allowedOrigins := []string{"*"} // Default to restrictive in production
	if origins != "" {
		allowedOrigins = strings.Split(origins, ",")
	}

	externalURL := os.Getenv("EXTERNAL_SERVICE_URL")
	if externalURL == "" {
		externalURL = "https://api.external-service.com/v1/verify"
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://user:pass@localhost:5432/dbname?sslmode=disable"
	}

	return Config{
		Port:               port,
		ProjectID:          os.Getenv("GOOGLE_CLOUD_PROJECT"),
		AllowedOrigins:     allowedOrigins,
		ExternalServiceURL: externalURL,
		DatabaseURL:        dbURL,
	}
}

func initFirebase(ctx context.Context, projectID string) appMiddleware.TokenVerifier {
	if host := os.Getenv("FIREBASE_AUTH_EMULATOR_HOST"); host != "" {
		slog.Info("Running with Firebase Auth Emulator", "host", host)
	}

	conf := &firebase.Config{ProjectID: projectID}
	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		slog.Error("error initializing firebase app", "error", err)
		os.Exit(1)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		slog.Error("error getting firebase auth client", "error", err)
		os.Exit(1)
	}
	return client
}

func initTracer(projectID string) (*sdktrace.TracerProvider, error) {
	exporter, err := texporter.New(texporter.WithProjectID(projectID))
	if err != nil {
		return nil, fmt.Errorf("unable to initialize google cloud trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("contract-record-keeper"),
			semconv.DeploymentEnvironmentKey.String(os.Getenv("ENV")),
		)),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}

func main() {
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tp, err := initTracer(cfg.ProjectID)
	if err != nil {
		slog.Error("failed to initialize tracer", "error", err)
		os.Exit(1)
	}
	defer tp.Shutdown(context.Background())

	baseHandler := slog.NewJSONHandler(os.Stdout, nil)
	logger := slog.New(appMiddleware.NewContextHandler(baseHandler))
	slog.SetDefault(logger)

	authClient := initFirebase(ctx, cfg.ProjectID)

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}

	// Initialize Circuit Breaker for external dependencies
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name: "ExternalService",
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 3 && failureRatio >= 0.6
		},
	})

	// Centralized HTTP client with OpenTelemetry instrumentation and reasonable timeouts
	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}

	h := &handlers.Server{
		Auth:               authClient,
		CB:                 cb,
		Client:             httpClient,
		DB:                 db,
		ExternalServiceURL: cfg.ExternalServiceURL,
	}

	r := chi.NewRouter()

	r.Use(otelchi.Middleware("backend-service"))
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(appMiddleware.JSONLoggingMiddleware)
	r.Use(appMiddleware.CORS(cfg.AllowedOrigins))

	r.NotFound(handlers.NotFoundHandler)

	r.Get("/health", h.HealthCheck)
	r.Get("/ready", h.ReadyCheck)
	r.Handle("/metrics", promhttp.Handler())

	r.Group(func(r chi.Router) {
		r.Use(appMiddleware.VerifyFirebaseToken(authClient))
		r.Get("/record", h.GetRecordHandler)
		r.Get("/", h.RootHandler)
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
	}

	go func() {
		slog.Info("Starting server", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("Shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}
}
