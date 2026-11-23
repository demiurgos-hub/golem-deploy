package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var (
	// DB is the global database connection
	DB *sql.DB
)

// Init initializes the database connection and creates tables if needed
func Init(dbPath string) error {
	// Ensure the directory exists
	dbDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	// Open database connection
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	DB = db

	// Create accounts table if it doesn't exist
	if err := createTables(); err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	fmt.Println("Database initialized successfully")
	return nil
}

// createTables creates the necessary database tables
func createTables() error {
	query := `
	CREATE TABLE IF NOT EXISTS accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		access_level INTEGER DEFAULT 1,
		approved INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_username ON accounts(username);

	CREATE TABLE IF NOT EXISTS repositories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		url TEXT NOT NULL,
		branch TEXT DEFAULT 'main',
		auth_type TEXT NOT NULL DEFAULT 'none',
		username TEXT,
		password TEXT,
		ssh_key TEXT,
		last_commit TEXT,
		last_pulled DATETIME,
		status TEXT DEFAULT 'idle',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_repo_name ON repositories(name);

	CREATE TABLE IF NOT EXISTS repo_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL,
		kind TEXT NOT NULL,
		status TEXT NOT NULL,
		detail TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		finished_at DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_repo_jobs_repo_id ON repo_jobs(repo_id);

	CREATE TABLE IF NOT EXISTS api_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL,
		name TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		prefix TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_api_tokens_username ON api_tokens(username);
	CREATE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens(token_hash);
	`

	if _, err := DB.Exec(query); err != nil {
		return err
	}

	// Run migrations to add new columns to existing tables
	return runMigrations()
}

// runMigrations adds new columns to existing tables if they don't exist
func runMigrations() error {
	// Check if status column exists in repositories table
	var columnExists bool
	err := DB.QueryRow(`
		SELECT COUNT(*) > 0 
		FROM pragma_table_info('repositories') 
		WHERE name='status'
	`).Scan(&columnExists)

	if err != nil {
		return fmt.Errorf("failed to check for status column: %w", err)
	}

	// Add status column if it doesn't exist
	if !columnExists {
		_, err := DB.Exec("ALTER TABLE repositories ADD COLUMN status TEXT DEFAULT 'idle'")
		if err != nil {
			return fmt.Errorf("failed to add status column: %w", err)
		}
		fmt.Println("Added status column to repositories table")
	}

	// Check if access_level column exists in accounts table
	var accessLevelExists bool
	err = DB.QueryRow(`
		SELECT COUNT(*) > 0 
		FROM pragma_table_info('accounts') 
		WHERE name='access_level'
	`).Scan(&accessLevelExists)

	if err != nil {
		return fmt.Errorf("failed to check for access_level column: %w", err)
	}

	// Add access_level column if it doesn't exist
	if !accessLevelExists {
		_, err := DB.Exec("ALTER TABLE accounts ADD COLUMN access_level INTEGER DEFAULT 1")
		if err != nil {
			return fmt.Errorf("failed to add access_level column: %w", err)
		}
		fmt.Println("Added access_level column to accounts table")

		// Set the first account to admin (access_level = 0) for existing databases
		_, err = DB.Exec("UPDATE accounts SET access_level = 0 WHERE id = (SELECT MIN(id) FROM accounts)")
		if err != nil {
			return fmt.Errorf("failed to set first account as admin: %w", err)
		}
		fmt.Println("Set first account as admin")
	}

	// Check if approved column exists in accounts table
	var approvedExists bool
	err = DB.QueryRow(`
		SELECT COUNT(*) > 0 
		FROM pragma_table_info('accounts') 
		WHERE name='approved'
	`).Scan(&approvedExists)

	if err != nil {
		return fmt.Errorf("failed to check for approved column: %w", err)
	}

	// Add approved column if it doesn't exist
	if !approvedExists {
		_, err := DB.Exec("ALTER TABLE accounts ADD COLUMN approved INTEGER DEFAULT 0")
		if err != nil {
			return fmt.Errorf("failed to add approved column: %w", err)
		}
		fmt.Println("Added approved column to accounts table")

		// Approve all existing accounts so they can continue logging in
		_, err = DB.Exec("UPDATE accounts SET approved = 1")
		if err != nil {
			return fmt.Errorf("failed to approve existing accounts: %w", err)
		}
		fmt.Println("Approved all existing accounts")
	}

	return nil
}

// AddAccount adds a new account with a hashed password
func AddAccount(username, password string, accessLevel int, approved bool) error {
	// Hash the password using bcrypt
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Convert bool to int for SQLite
	approvedInt := 0
	if approved {
		approvedInt = 1
	}

	// Insert into database
	_, err = DB.Exec("INSERT INTO accounts (username, password_hash, access_level, approved) VALUES (?, ?, ?, ?)",
		username, string(hashedPassword), accessLevel, approvedInt)
	if err != nil {
		return fmt.Errorf("failed to insert account: %w", err)
	}

	return nil
}

// ValidateCredentials checks if the username and password are valid
// Returns (valid, accessLevel, error)
func ValidateCredentials(username, password string) (bool, int, error) {
	var passwordHash string
	var accessLevel int
	var approvedInt int

	err := DB.QueryRow("SELECT password_hash, access_level, approved FROM accounts WHERE username = ?", username).
		Scan(&passwordHash, &accessLevel, &approvedInt)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, 0, nil // User not found
		}
		return false, 0, fmt.Errorf("database error: %w", err)
	}

	// Check if account is approved
	if approvedInt == 0 {
		return false, 0, fmt.Errorf("account not approved")
	}

	// Compare password with hash
	err = bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password))
	if err != nil {
		return false, 0, nil // Invalid password
	}

	return true, accessLevel, nil
}

// IsApprovedAccount reports whether username exists and is approved.
func IsApprovedAccount(username string) (bool, error) {
	var approvedInt int
	err := DB.QueryRow("SELECT approved FROM accounts WHERE username = ?", username).Scan(&approvedInt)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("failed to check account: %w", err)
	}
	return approvedInt != 0, nil
}

// HasAccounts checks if there are any accounts in the database
func HasAccounts() (bool, error) {
	var count int
	err := DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to count accounts: %w", err)
	}
	return count > 0, nil
}

// UpdatePassword updates the password for a given username
func UpdatePassword(username, oldPassword, newPassword string) error {
	// First validate the old password
	valid, _, err := ValidateCredentials(username, oldPassword)
	if err != nil {
		return fmt.Errorf("failed to validate credentials: %w", err)
	}
	if !valid {
		return fmt.Errorf("invalid old password")
	}

	// Hash the new password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	// Update the password in database
	result, err := DB.Exec("UPDATE accounts SET password_hash = ? WHERE username = ?", string(hashedPassword), username)
	if err != nil {
		return fmt.Errorf("failed to update password: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("user not found")
	}

	return nil
}

// DeleteAccount deletes an account by username and its API tokens
func DeleteAccount(username string) error {
	tx, err := DB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM api_tokens WHERE username = ?", username); err != nil {
		return fmt.Errorf("failed to delete account tokens: %w", err)
	}

	result, err := tx.Exec("DELETE FROM accounts WHERE username = ?", username)
	if err != nil {
		return fmt.Errorf("failed to delete account: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("user not found")
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account delete: %w", err)
	}

	return nil
}

// CountAccounts returns the total number of accounts
func CountAccounts() (int, error) {
	var count int
	err := DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count accounts: %w", err)
	}
	return count, nil
}

// Account represents an account from the database
type Account struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	AccessLevel int    `json:"access_level"`
	Approved    bool   `json:"approved"`
	CreatedAt   string `json:"created_at"`
}

// GetUnapprovedAccounts returns all accounts that are not approved
func GetUnapprovedAccounts() ([]Account, error) {
	rows, err := DB.Query("SELECT id, username, access_level, approved, created_at FROM accounts WHERE approved = 0 ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("failed to query unapproved accounts: %w", err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var account Account
		var approvedInt int
		err := rows.Scan(&account.ID, &account.Username, &account.AccessLevel, &approvedInt, &account.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan account: %w", err)
		}
		account.Approved = approvedInt == 1
		accounts = append(accounts, account)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating accounts: %w", err)
	}

	return accounts, nil
}

// GetAllAccounts returns all accounts except the first admin account
func GetAllAccounts() ([]Account, error) {
	// Get the first admin account ID (the one with the lowest ID and access_level = 0)
	var firstAdminID int
	err := DB.QueryRow("SELECT id FROM accounts WHERE access_level = 0 ORDER BY id ASC LIMIT 1").Scan(&firstAdminID)
	if err != nil {
		return nil, fmt.Errorf("failed to get first admin ID: %w", err)
	}

	// Get all accounts except the first admin
	rows, err := DB.Query("SELECT id, username, access_level, approved, created_at FROM accounts WHERE id != ? ORDER BY username ASC", firstAdminID)
	if err != nil {
		return nil, fmt.Errorf("failed to query accounts: %w", err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var account Account
		var approvedInt int
		err := rows.Scan(&account.ID, &account.Username, &account.AccessLevel, &approvedInt, &account.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan account: %w", err)
		}
		account.Approved = approvedInt == 1
		accounts = append(accounts, account)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating accounts: %w", err)
	}

	return accounts, nil
}

// ApproveAccount sets the approved status for an account
func ApproveAccount(username string) error {
	result, err := DB.Exec("UPDATE accounts SET approved = 1 WHERE username = ?", username)
	if err != nil {
		return fmt.Errorf("failed to approve account: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("account not found")
	}

	return nil
}

// Close closes the database connection
func Close() error {
	if DB != nil {
		return DB.Close()
	}
	return nil
}
