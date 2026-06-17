// Package main is a runnable example backend API that wires the go-clients
// email, storage, and kms modules behind a small Gin HTTP service.
//
// It is intended as a reference for how to construct and use the clients in a
// real service, including request authentication, body-size limits, request
// timeouts, and error handling that never leaks provider internals or secrets
// to callers. It is not meant to be deployed as-is.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// config holds all runtime configuration, loaded from the environment.
type config struct {
	Addr     string
	APIKey   string
	LogLevel string

	EmailProvider string // "ses" | "postmark"
	SES           sesConfig
	Postmark      postmarkConfig

	StorageProvider string // "gcs" | "r2"
	GCS             gcsConfig
	R2              r2Config

	KMS kmsConfig

	// MaxRequestBytes caps the size of an incoming request body.
	MaxRequestBytes int64
}

type sesConfig struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
}

type postmarkConfig struct {
	ServerToken string
}

type gcsConfig struct {
	ServiceAccount string // base64 JSON, optional (ADC if empty)
	Bucket         string
}

type r2Config struct {
	AccountID   string
	AccessKeyID string
	SecretKey   string
	Bucket      string
}

type kmsConfig struct {
	ServiceAccount string // base64 JSON, optional (ADC if empty)
	KeyName        string
}

const defaultMaxRequestBytes = 10 << 20 // 10 MiB

// loadConfig reads configuration from the environment, returning an error that
// names every missing or invalid required variable at once. Secret values are
// never included in the error text.
func loadConfig() (*config, error) {
	var missing []string
	req := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	c := &config{
		Addr:            getenvDefault("ADDR", ":8080"),
		APIKey:          req("API_KEY"),
		LogLevel:        getenvDefault("LOG_LEVEL", "info"),
		EmailProvider:   strings.ToLower(getenvDefault("EMAIL_PROVIDER", "postmark")),
		StorageProvider: strings.ToLower(getenvDefault("STORAGE_PROVIDER", "gcs")),
		MaxRequestBytes: defaultMaxRequestBytes,
		KMS: kmsConfig{
			ServiceAccount: os.Getenv("KMS_SERVICE_ACCOUNT"),
			KeyName:        req("KMS_KEY_NAME"),
		},
	}

	switch c.EmailProvider {
	case "ses":
		c.SES = sesConfig{
			AccessKeyID:     req("SES_ACCESS_KEY_ID"),
			SecretAccessKey: req("SES_SECRET_ACCESS_KEY"),
			Region:          req("SES_REGION"),
		}
	case "postmark":
		c.Postmark = postmarkConfig{ServerToken: req("POSTMARK_SERVER_TOKEN")}
	default:
		return nil, fmt.Errorf("invalid EMAIL_PROVIDER %q (want ses or postmark)", c.EmailProvider)
	}

	switch c.StorageProvider {
	case "gcs":
		c.GCS = gcsConfig{
			ServiceAccount: os.Getenv("GCS_SERVICE_ACCOUNT"),
			Bucket:         req("GCS_BUCKET"),
		}
	case "r2":
		c.R2 = r2Config{
			AccountID:   req("R2_ACCOUNT_ID"),
			AccessKeyID: req("R2_ACCESS_KEY_ID"),
			SecretKey:   req("R2_SECRET_KEY"),
			Bucket:      req("R2_BUCKET"),
		}
	default:
		return nil, fmt.Errorf("invalid STORAGE_PROVIDER %q (want gcs or r2)", c.StorageProvider)
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if len(c.APIKey) < 16 {
		return nil, errors.New("API_KEY must be at least 16 characters")
	}

	return c, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
