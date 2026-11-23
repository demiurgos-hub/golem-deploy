package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the application configuration.
//
// The YAML file at the configured path is optional and currently defines no
// keys; unknown keys are ignored. The JWT secret always lives in a separate
// jwt_secret file next to the configured path.
type Config struct {
	// JWTSecret is the secret key used to sign JWT tokens
	// This is loaded from a separate jwt_secret file, not from the YAML config
	JWTSecret string `yaml:"-"`
}

var (
	// Global holds the loaded configuration
	Global *Config
)

// Load reads and parses the configuration file
func Load(configPath string) (*Config, error) {
	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Return default empty config if file doesn't exist
		fmt.Printf("Config file not found at %s, using defaults\n", configPath)

		// Still need to load JWT secret from separate file
		jwtSecretPath := filepath.Join(filepath.Dir(configPath), "jwt_secret")
		jwtSecret, err := loadOrGenerateJWTSecret(jwtSecretPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load JWT secret: %w", err)
		}

		return &Config{JWTSecret: jwtSecret}, nil
	}

	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Load JWT secret from separate file
	jwtSecretPath := filepath.Join(filepath.Dir(configPath), "jwt_secret")
	jwtSecret, err := loadOrGenerateJWTSecret(jwtSecretPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load JWT secret: %w", err)
	}
	cfg.JWTSecret = jwtSecret

	return &cfg, nil
}

// generateJWTSecret creates a cryptographically secure random JWT secret
func generateJWTSecret() (string, error) {
	// Generate 32 bytes (256 bits) of random data
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	// Encode to base64 for safe storage
	return base64.URLEncoding.EncodeToString(bytes), nil
}

// loadOrGenerateJWTSecret loads the JWT secret from file, or generates a new one if it doesn't exist
func loadOrGenerateJWTSecret(secretPath string) (string, error) {
	// Check if secret file exists
	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		// Generate new secret
		fmt.Printf("JWT secret file not found at %s, generating new secret...\n", secretPath)
		secret, err := generateJWTSecret()
		if err != nil {
			return "", err
		}

		// Write secret to file
		if err := os.WriteFile(secretPath, []byte(secret), 0600); err != nil {
			return "", fmt.Errorf("failed to write JWT secret file: %w", err)
		}

		fmt.Printf("Generated and saved new JWT secret to %s\n", secretPath)
		return secret, nil
	}

	// Read existing secret
	data, err := os.ReadFile(secretPath)
	if err != nil {
		return "", fmt.Errorf("failed to read JWT secret file: %w", err)
	}

	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("JWT secret file is empty")
	}

	return secret, nil
}

// Init initializes the global configuration
func Init(configPath string) error {
	cfg, err := Load(configPath)
	if err != nil {
		return err
	}

	Global = cfg
	return nil
}
