package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"syscall"

	"github.com/demiurgos-hub/golem-deploy/frontend"
	"github.com/demiurgos-hub/golem-deploy/internal/config"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/account"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/docker/container"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/docker/image"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/docker/volume"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/massrun"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/repo"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/static"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"

	"golang.org/x/term"
)

func main() {
	// Load configuration
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "./config.yaml"
	}
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize database
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/accounts.db"
	}
	if err := database.Init(dbPath); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	// Initialize mass run database
	massRunDBPath := os.Getenv("MASSRUN_DB_PATH")
	if massRunDBPath == "" {
		massRunDBPath = "./data/massrun.db"
	}
	if err := database.InitMassRun(massRunDBPath); err != nil {
		log.Fatalf("Failed to initialize mass run database: %v", err)
	}
	defer database.CloseMassRun()

	// Set JWT secret from config
	middleware.SetJWTSecret(config.Global.JWTSecret)

	// Check if any accounts exist, if not, prompt for admin account creation
	hasAccounts, err := database.HasAccounts()
	if err != nil {
		log.Fatalf("Failed to check accounts: %v", err)
	}
	if !hasAccounts {
		fmt.Println("\033[33m=== First Time Setup ===\033[0m")
		fmt.Println("No accounts found. Create your first admin account now.")
		fmt.Println()
		if err := promptCreateAdminAccount(); err != nil {
			log.Fatalf("Failed to create admin account: %v", err)
		}
		fmt.Println("\033[32m✓ Admin account created successfully!\033[0m")
		fmt.Println()
	}

	// Initialize the uploads directory (static file store)
	if err := utils.Init(); err != nil {
		log.Fatalf("Failed to initialize: %v", err)
	}

	// Register HTTP handlers
	// Public endpoints (no authentication required)
	http.HandleFunc("/login", handlers.LoginHandler)

	// Protected endpoints (authentication required)
	http.HandleFunc("/status", middleware.AuthMiddleware(handlers.StatusHandler))

	// Public account endpoints
	http.HandleFunc("/account/create", account.CreateHandler)

	// Protected account endpoints
	http.HandleFunc("/account/refresh", middleware.AuthMiddleware(account.RefreshHandler))
	http.HandleFunc("/account/password", middleware.AuthMiddleware(account.PasswordHandler))
	http.HandleFunc("/account/delete", middleware.AuthMiddleware(account.DeleteHandler))
	http.HandleFunc("/account/approve", middleware.AuthMiddleware(account.ApproveHandler))
	http.HandleFunc("/account/unapproved", middleware.AuthMiddleware(account.ListUnapprovedHandler))
	http.HandleFunc("/account/list", middleware.AuthMiddleware(account.ListAllHandler))
	http.HandleFunc("/account/tokens", middleware.AuthMiddleware(account.TokensHandler))
	http.HandleFunc("/account/tokens/revoke", middleware.AuthMiddleware(account.RevokeTokenHandler))

	// Static file endpoints
	http.HandleFunc("/build/static/upload", middleware.AuthMiddleware(static.UploadHandler))
	http.HandleFunc("/build/static/list", middleware.AuthMiddleware(static.ListHandler))
	http.HandleFunc("/build/static/delete", middleware.AuthMiddleware(static.DeleteHandler))
	http.HandleFunc("/build/static/download", middleware.AuthMiddleware(static.DownloadHandler))

	http.HandleFunc("/docker/image/list", middleware.AuthMiddleware(image.ListHandler))
	http.HandleFunc("/docker/image/delete", middleware.AuthMiddleware(image.DeleteHandler))

	http.HandleFunc("/docker/container/run", middleware.AuthMiddleware(container.RunHandler))
	http.HandleFunc("/docker/container/start", middleware.AuthMiddleware(container.StartHandler))
	http.HandleFunc("/docker/container/stop", middleware.AuthMiddleware(container.StopHandler))
	http.HandleFunc("/docker/container/delete", middleware.AuthMiddleware(container.DeleteHandler))
	http.HandleFunc("/docker/container/list", middleware.AuthMiddleware(container.ListHandler))
	http.HandleFunc("/docker/container/logs", middleware.AuthMiddleware(container.LogsHandler))
	http.HandleFunc("/docker/container/processes", middleware.AuthMiddleware(container.ProcessesHandler))

	http.HandleFunc("/docker/volume/list", middleware.AuthMiddleware(volume.ListHandler))
	http.HandleFunc("/docker/volume/orphaned", middleware.AuthMiddleware(volume.OrphanedHandler))
	http.HandleFunc("/docker/volume/delete", middleware.AuthMiddleware(volume.DeleteHandler))
	http.HandleFunc("/docker/volume/info", middleware.AuthMiddleware(volume.InfoHandler))

	http.HandleFunc("/massrun/add", middleware.AuthMiddleware(massrun.AddHandler))
	http.HandleFunc("/massrun/edit", middleware.AuthMiddleware(massrun.EditHandler))
	http.HandleFunc("/massrun/list", middleware.AuthMiddleware(massrun.ListHandler))
	http.HandleFunc("/massrun/delete", middleware.AuthMiddleware(massrun.DeleteHandler))
	http.HandleFunc("/massrun/replace", middleware.AuthMiddleware(massrun.ReplaceHandler))
	http.HandleFunc("/massrun/execute", middleware.AuthMiddleware(massrun.ExecuteHandler))

	// Repository endpoints
	http.HandleFunc("/repo/add", middleware.AuthMiddleware(repo.AddHandler))
	http.HandleFunc("/repo/list", middleware.AuthMiddleware(repo.ListHandler))
	http.HandleFunc("/repo/pull", middleware.AuthMiddleware(repo.PullHandler))
	http.HandleFunc("/repo/delete", middleware.AuthMiddleware(repo.DeleteHandler))
	http.HandleFunc("/repo/branch", middleware.AuthMiddleware(repo.BranchHandler))
	http.HandleFunc("/repo/job", middleware.AuthMiddleware(repo.JobHandler))

	// Serve the embedded frontend for all other routes (SPA fallback)
	http.Handle("/", frontend.Handler())

	// Get certificate paths from environment variables
	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")

	// If certificates are not provided, fall back to HTTP (for development only)
	if certFile == "" || keyFile == "" {
		log.Println("\033[31mWARNING: TLS certificates not configured. Running in HTTP mode (NOT RECOMMENDED FOR PRODUCTION)\033[0m")
		log.Println("\033[31mSet TLS_CERT_FILE and TLS_KEY_FILE environment variables to enable HTTPS\033[0m")

		// Listen on all interfaces on port 8000
		fmt.Println("Listening on :8000 (HTTP)")

		if err := http.ListenAndServe(":8000", nil); err != nil {
			log.Fatalf("Failed to start server: %v", err)
		}
	} else {
		// Verify certificate files exist
		if _, err := os.Stat(certFile); os.IsNotExist(err) {
			log.Fatalf("TLS certificate file not found: %s", certFile)
		}
		if _, err := os.Stat(keyFile); os.IsNotExist(err) {
			log.Fatalf("TLS key file not found: %s", keyFile)
		}

		// Listen on HTTPS (port 8000)
		fmt.Println("Listening on :8000 (HTTPS)")
		fmt.Printf("Using TLS certificate: %s\n", certFile)
		fmt.Printf("Using TLS key: %s\n", keyFile)

		if err := http.ListenAndServeTLS(":8000", certFile, keyFile, nil); err != nil {
			log.Fatalf("Failed to start HTTPS server: %v", err)
		}
	}
}

// promptCreateAdminAccount interactively prompts the user to create an admin account
func promptCreateAdminAccount() error {
	reader := bufio.NewReader(os.Stdin)

	// Prompt for username
	fmt.Print("Enter admin username: ")
	username, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read username: %w", err)
	}
	username = strings.TrimSpace(username)

	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}

	// Prompt for password (hidden)
	fmt.Print("Enter password (min 8 characters): ")
	passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}
	password := string(passwordBytes)
	fmt.Println() // New line after password input

	// Validate password length
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters long")
	}

	// Prompt for password confirmation
	fmt.Print("Confirm password: ")
	confirmBytes, err := term.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return fmt.Errorf("failed to read password confirmation: %w", err)
	}
	confirm := string(confirmBytes)
	fmt.Println() // New line after password input

	// Check if passwords match
	if password != confirm {
		return fmt.Errorf("passwords do not match")
	}

	// Add account to database as admin (access_level=0) and approved
	if err := database.AddAccount(username, password, 0, true); err != nil {
		return fmt.Errorf("failed to add account: %w", err)
	}

	return nil
}
