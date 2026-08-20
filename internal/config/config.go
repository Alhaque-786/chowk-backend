// Package config loads and validates all runtime configuration from the
// environment. Nothing else in the codebase should read os.Getenv directly --
// that keeps every knob discoverable in one place and makes the process fail
// fast at boot instead of at the first request.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Env string

const (
	EnvLocal      Env = "local"
	EnvStaging    Env = "staging"
	EnvProduction Env = "production"
)

type Config struct {
	Env      Env
	Server   ServerConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	Auth     AuthConfig
	Payment  PaymentConfig
	Storage  StorageConfig
	Observ   ObservabilityConfig
}

type ServerConfig struct {
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	AllowedOrigins  []string
}

type PostgresConfig struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type AuthConfig struct {
	JWTSecret          string
	AccessTokenTTL     time.Duration
	RefreshTokenTTL    time.Duration
	OTPTTL             time.Duration
	OTPMaxAttempts     int
	BcryptCost         int
	PasswordMinEntropy int
}

// PaymentConfig holds gateway credentials. Razorpay is the default for India;
// swap the provider behind the payment.Gateway interface if that changes.
type PaymentConfig struct {
	Provider      string
	KeyID         string
	KeySecret     string
	WebhookSecret string
}

type StorageConfig struct {
	Provider  string // "s3" | "local"
	Bucket    string
	Region    string
	PublicURL string
}

type ObservabilityConfig struct {
	LogLevel  string
	LogFormat string // "json" | "text"
}

// Load reads configuration from the environment. It returns an error listing
// every problem it found rather than stopping at the first one, so a
// misconfigured deploy surfaces all its issues in a single log line.
func Load() (*Config, error) {
	var problems []string

	require := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			problems = append(problems, fmt.Sprintf("%s is required", key))
		}
		return v
	}

	env := Env(optString("APP_ENV", string(EnvLocal)))
	switch env {
	case EnvLocal, EnvStaging, EnvProduction:
	default:
		problems = append(problems, fmt.Sprintf("APP_ENV must be one of local|staging|production, got %q", env))
	}

	cfg := &Config{
		Env: env,
		Server: ServerConfig{
			Port:            optInt("PORT", 8080),
			ReadTimeout:     optDuration("SERVER_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    optDuration("SERVER_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:     optDuration("SERVER_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout: optDuration("SERVER_SHUTDOWN_TIMEOUT", 20*time.Second),
			AllowedOrigins:  optCSV("CORS_ALLOWED_ORIGINS", []string{"http://localhost:3000"}),
		},
		Postgres: PostgresConfig{
			DSN:             require("POSTGRES_DSN"),
			MaxConns:        int32(optInt("POSTGRES_MAX_CONNS", 25)),
			MinConns:        int32(optInt("POSTGRES_MIN_CONNS", 2)),
			MaxConnLifetime: optDuration("POSTGRES_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: optDuration("POSTGRES_MAX_CONN_IDLE_TIME", 30*time.Minute),
		},
		Redis: RedisConfig{
			Addr:     optString("REDIS_ADDR", "localhost:6379"),
			Password: os.Getenv("REDIS_PASSWORD"),
			DB:       optInt("REDIS_DB", 0),
		},
		Auth: AuthConfig{
			JWTSecret:       require("JWT_SECRET"),
			AccessTokenTTL:  optDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL: optDuration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
			OTPTTL:          optDuration("OTP_TTL", 5*time.Minute),
			OTPMaxAttempts:  optInt("OTP_MAX_ATTEMPTS", 5),
			BcryptCost:      optInt("BCRYPT_COST", 12),
		},
		Payment: PaymentConfig{
			Provider:      optString("PAYMENT_PROVIDER", "razorpay"),
			KeyID:         os.Getenv("PAYMENT_KEY_ID"),
			KeySecret:     os.Getenv("PAYMENT_KEY_SECRET"),
			WebhookSecret: os.Getenv("PAYMENT_WEBHOOK_SECRET"),
		},
		Storage: StorageConfig{
			Provider:  optString("STORAGE_PROVIDER", "local"),
			Bucket:    os.Getenv("STORAGE_BUCKET"),
			Region:    optString("STORAGE_REGION", "ap-south-1"),
			PublicURL: optString("STORAGE_PUBLIC_URL", "http://localhost:8080/static"),
		},
		Observ: ObservabilityConfig{
			LogLevel:  optString("LOG_LEVEL", "info"),
			LogFormat: optString("LOG_FORMAT", "json"),
		},
	}

	// A short secret is worse than no secret because it looks configured.
	if len(cfg.Auth.JWTSecret) > 0 && len(cfg.Auth.JWTSecret) < 32 {
		problems = append(problems, "JWT_SECRET must be at least 32 characters")
	}
	if cfg.Env == EnvProduction {
		if cfg.Payment.KeySecret == "" {
			problems = append(problems, "PAYMENT_KEY_SECRET is required in production")
		}
		if cfg.Payment.WebhookSecret == "" {
			problems = append(problems, "PAYMENT_WEBHOOK_SECRET is required in production")
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func (c *Config) IsProduction() bool { return c.Env == EnvProduction }
func (c *Config) IsLocal() bool      { return c.Env == EnvLocal }

func optString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func optInt(key string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return v
}

func optDuration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return v
}

func optCSV(key string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
