package config

import (
	"os"

	"github.com/joho/godotenv"
)

// Settings mirrors the Python Settings, with sane defaults and env overrides.
type Settings struct {
	AppTitle       string
	AppDescription string
	AppVersion     string

	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string

	S3BucketName string
	S3Endpoint   string
	S3AccessKey  string
	S3SecretKey  string
	S3Region     string
}

// Load loads settings from environment variables, using .env or .env.test if present.
// If RUN_HANDLER_ENV=test, it attempts to load .env.test first; otherwise .env.
func Load() Settings {
	env := os.Getenv("RUN_HANDLER_ENV")
	if env == "test" {
		_ = godotenv.Load(".env.test")
	} else {
		_ = godotenv.Load(".env")
	}

	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}

	return Settings{
		AppTitle:       get("APP_TITLE", "LS Run Handler"),
		AppDescription: get("APP_DESCRIPTION", "A simple Go server with run endpoints"),
		AppVersion:     get("APP_VERSION", "0.1.0"),

		DBHost:     get("DB_HOST", "localhost"),
		DBPort:     get("DB_PORT", "5432"),
		DBUser:     get("DB_USER", "postgres"),
		DBPassword: get("DB_PASSWORD", "postgres"),
		DBName:     get("DB_NAME", "postgres"),

		S3BucketName: get("S3_BUCKET_NAME", "runs"),
		S3Endpoint:   get("S3_ENDPOINT_URL", "http://localhost:9002"),
		S3AccessKey:  get("S3_ACCESS_KEY", "minioadmin1"),
		S3SecretKey:  get("S3_SECRET_KEY", "minioadmin1"),
		S3Region:     get("S3_REGION", "us-east-1"),
	}
}
