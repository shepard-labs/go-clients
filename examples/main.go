package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/email"
	"github.com/shepard-labs/go-clients/email/postmark"
	"github.com/shepard-labs/go-clients/email/ses"
	"github.com/shepard-labs/go-clients/kms"
	"github.com/shepard-labs/go-clients/storage"
	"github.com/shepard-labs/go-clients/storage/gcs"
	"github.com/shepard-labs/go-clients/storage/r2"
)

const serviceTag = "go-clients-example"

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatal("configuration error", zap.Error(err))
	}

	ctx := context.Background()

	sender, err := buildSender(cfg, logger)
	if err != nil {
		logger.Fatal("failed to build email sender", zap.Error(err))
	}

	store, err := buildStorage(ctx, cfg, logger)
	if err != nil {
		logger.Fatal("failed to build storage client", zap.Error(err))
	}
	defer func() { _ = store.Close() }()

	encryptor, err := kms.New(ctx, cfg.KMS.ServiceAccount, cfg.KMS.KeyName, logger)
	if err != nil {
		logger.Fatal("failed to build kms client", zap.Error(err))
	}
	defer func() { _ = encryptor.Close() }()

	srv := &server{logger: logger, sender: sender, store: store, encryptor: encryptor}
	router := buildRouter(cfg, srv)

	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: router,
		// Timeouts guard against slow-client (Slowloris-style) resource exhaustion.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("listening", zap.String("addr", cfg.Addr),
			zap.String("email_provider", cfg.EmailProvider),
			zap.String("storage_provider", cfg.StorageProvider))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", zap.Error(err))
	}
	logger.Info("server stopped")
}

func buildSender(cfg *config, logger *zap.Logger) (email.Sender, error) {
	switch cfg.EmailProvider {
	case "ses":
		return ses.New(ses.Credentials{
			AccessKeyID:     cfg.SES.AccessKeyID,
			SecretAccessKey: cfg.SES.SecretAccessKey,
			Region:          cfg.SES.Region,
		}, serviceTag, logger), nil
	case "postmark":
		return postmark.New(cfg.Postmark.ServerToken, logger), nil
	default:
		return nil, errors.New("unknown email provider")
	}
}

func buildStorage(ctx context.Context, cfg *config, logger *zap.Logger) (storage.Storage, error) {
	switch cfg.StorageProvider {
	case "gcs":
		return gcs.New(ctx, cfg.GCS.ServiceAccount, cfg.GCS.Bucket, serviceTag, 0, logger)
	case "r2":
		return r2.New(cfg.R2.AccountID, cfg.R2.AccessKeyID, cfg.R2.SecretKey, cfg.R2.Bucket, serviceTag, 0, logger)
	default:
		return nil, errors.New("unknown storage provider")
	}
}

func buildRouter(cfg *config, srv *server) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery()) // converts panics into 500s instead of crashing

	// Health is unauthenticated for liveness/readiness probes.
	r.GET("/healthz", srv.health)

	api := r.Group("/")
	api.Use(
		limitBody(cfg.MaxRequestBytes),
		withTimeout(25*time.Second),
		bearerAuth(cfg.APIKey),
	)
	{
		api.POST("/email/send", srv.sendEmail)
		api.POST("/storage/upload", srv.upload)
		api.GET("/storage/download", srv.download)
		api.POST("/kms/encrypt", srv.encrypt)
		api.POST("/kms/decrypt", srv.decrypt)
	}

	return r
}
