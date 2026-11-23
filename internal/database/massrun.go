package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var (
	// MassRunDB is the database connection for mass run configurations
	MassRunDB *sql.DB
)

// MassRunConfig represents a saved container run configuration
type MassRunConfig struct {
	ID            int               `json:"id"`
	Name          string            `json:"name"`
	ContainerName string            `json:"container_name"`
	Port          string            `json:"port"`
	Label         string            `json:"label"`
	EnvVars       map[string]string `json:"env_vars"`
	CreatedAt     string            `json:"created_at"`
}

// InitMassRun initializes the mass run database connection and creates tables if needed
func InitMassRun(dbPath string) error {
	// Ensure the directory exists
	dbDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	// Open database connection
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open mass run database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping mass run database: %w", err)
	}

	MassRunDB = db

	// Create mass run configurations table if it doesn't exist
	if err := createMassRunTables(); err != nil {
		return fmt.Errorf("failed to create mass run tables: %w", err)
	}

	fmt.Println("Mass run database initialized successfully")
	return nil
}

// createMassRunTables creates the necessary database tables
func createMassRunTables() error {
	query := `
	CREATE TABLE IF NOT EXISTS mass_run_configs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		container_name TEXT NOT NULL,
		port TEXT,
		label TEXT,
		env_vars TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_mass_run_name ON mass_run_configs(name);
	`

	_, err := MassRunDB.Exec(query)
	return err
}

// AddMassRunConfig adds a new mass run configuration
func AddMassRunConfig(config *MassRunConfig) error {
	// Convert env vars map to JSON string
	envVarsJSON, err := json.Marshal(config.EnvVars)
	if err != nil {
		return fmt.Errorf("failed to marshal env vars: %w", err)
	}

	query := `INSERT INTO mass_run_configs 
		(name, container_name, port, label, env_vars) 
		VALUES (?, ?, ?, ?, ?)`

	_, err = MassRunDB.Exec(query,
		config.Name,
		config.ContainerName,
		config.Port,
		config.Label,
		string(envVarsJSON),
	)
	if err != nil {
		return fmt.Errorf("failed to insert mass run config: %w", err)
	}

	return nil
}

// ListMassRunConfigs retrieves all mass run configurations
func ListMassRunConfigs() ([]MassRunConfig, error) {
	query := `SELECT id, name, container_name, port, label, env_vars, created_at 
		FROM mass_run_configs ORDER BY created_at DESC`

	rows, err := MassRunDB.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query mass run configs: %w", err)
	}
	defer rows.Close()

	var configs []MassRunConfig
	for rows.Next() {
		var config MassRunConfig
		var envVarsJSON string

		err := rows.Scan(
			&config.ID,
			&config.Name,
			&config.ContainerName,
			&config.Port,
			&config.Label,
			&envVarsJSON,
			&config.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan mass run config: %w", err)
		}

		// Parse env vars JSON
		if envVarsJSON != "" {
			if err := json.Unmarshal([]byte(envVarsJSON), &config.EnvVars); err != nil {
				return nil, fmt.Errorf("failed to unmarshal env vars: %w", err)
			}
		} else {
			config.EnvVars = make(map[string]string)
		}

		configs = append(configs, config)
	}

	return configs, nil
}

// UpdateMassRunConfig updates an existing mass run configuration
func UpdateMassRunConfig(name string, config *MassRunConfig) error {
	// Convert env vars map to JSON string
	envVarsJSON, err := json.Marshal(config.EnvVars)
	if err != nil {
		return fmt.Errorf("failed to marshal env vars: %w", err)
	}

	query := `UPDATE mass_run_configs 
		SET container_name = ?, port = ?, label = ?, env_vars = ? 
		WHERE name = ?`

	result, err := MassRunDB.Exec(query,
		config.ContainerName,
		config.Port,
		config.Label,
		string(envVarsJSON),
		name,
	)
	if err != nil {
		return fmt.Errorf("failed to update mass run config: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("mass run config '%s' not found", name)
	}

	return nil
}

// DeleteMassRunConfig deletes a mass run configuration by name
func DeleteMassRunConfig(name string) error {
	query := `DELETE FROM mass_run_configs WHERE name = ?`
	result, err := MassRunDB.Exec(query, name)
	if err != nil {
		return fmt.Errorf("failed to delete mass run config: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("mass run config '%s' not found", name)
	}

	return nil
}

// DeleteMassRunConfigs deletes multiple mass run configurations by name.
func DeleteMassRunConfigs(names []string) error {
	for _, name := range names {
		if err := DeleteMassRunConfig(name); err != nil {
			return err
		}
	}

	return nil
}

// ReplaceMassRunConfigs atomically replaces all mass run configurations with the provided list.
func ReplaceMassRunConfigs(configs []*MassRunConfig) error {
	tx, err := MassRunDB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Exec(`DELETE FROM mass_run_configs`); err != nil {
		return fmt.Errorf("failed to clear mass run configs: %w", err)
	}

	query := `INSERT INTO mass_run_configs
		(name, container_name, port, label, env_vars)
		VALUES (?, ?, ?, ?, ?)`

	for _, config := range configs {
		envVars := config.EnvVars
		if envVars == nil {
			envVars = make(map[string]string)
		}

		envVarsJSON, err := json.Marshal(envVars)
		if err != nil {
			return fmt.Errorf("failed to marshal env vars: %w", err)
		}

		if _, err := tx.Exec(query,
			config.Name,
			config.ContainerName,
			config.Port,
			config.Label,
			string(envVarsJSON),
		); err != nil {
			return fmt.Errorf("failed to insert mass run config: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	committed = true

	return nil
}

// CloseMassRun closes the mass run database connection
func CloseMassRun() error {
	if MassRunDB != nil {
		return MassRunDB.Close()
	}
	return nil
}
