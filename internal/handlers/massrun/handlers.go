package massrun

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/docker/container"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

var runFromRequest = container.RunFromRequest

func validateMassRunRequest(req api.AddMassRunRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(req.ContainerName) == "" {
		return fmt.Errorf("container name is required")
	}
	return nil
}

func validateMassRunRequests(configs []api.AddMassRunRequest) error {
	seen := make(map[string]struct{}, len(configs))
	for i, config := range configs {
		if err := validateMassRunRequest(config); err != nil {
			return fmt.Errorf("config #%d: %w", i+1, err)
		}
		if _, exists := seen[config.Name]; exists {
			return fmt.Errorf("duplicate config name %q in request", config.Name)
		}
		seen[config.Name] = struct{}{}
	}
	return nil
}

// AddHandler handles requests to add a mass run configuration
func AddHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req api.AddMassRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if err := validateMassRunRequest(req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	// Create database config from request
	config := &database.MassRunConfig{
		Name:          req.Name,
		ContainerName: req.ContainerName,
		Port:          req.Port,
		Label:         req.Label,
		EnvVars:       req.EnvVars,
	}

	// Add to database
	if err := database.AddMassRunConfig(config); err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": fmt.Sprintf("Failed to add mass run configuration: %v", err),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "Mass run configuration added successfully",
	})
}

// EditHandler handles requests to edit an existing mass run configuration
func EditHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req api.EditMassRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if err := validateMassRunRequest(api.AddMassRunRequest(req)); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	// Create database config from request
	config := &database.MassRunConfig{
		ContainerName: req.ContainerName,
		Port:          req.Port,
		Label:         req.Label,
		EnvVars:       req.EnvVars,
	}

	// Update in database
	if err := database.UpdateMassRunConfig(req.Name, config); err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": fmt.Sprintf("Failed to update mass run configuration: %v", err),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "Mass run configuration updated successfully",
	})
}

// ListHandler handles requests to list all mass run configurations
func ListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	// Retrieve all configurations from database
	configs, err := database.ListMassRunConfigs()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": fmt.Sprintf("Failed to list mass run configurations: %v", err),
		})
		return
	}

	// Convert to API types
	apiConfigs := make([]api.MassRunConfig, len(configs))
	for i, config := range configs {
		apiConfigs[i] = api.MassRunConfig{
			ID:            config.ID,
			Name:          config.Name,
			ContainerName: config.ContainerName,
			Port:          config.Port,
			Label:         config.Label,
			EnvVars:       config.EnvVars,
			CreatedAt:     config.CreatedAt,
		}
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "success",
		"configs": apiConfigs,
	})
}

// DeleteHandler handles requests to delete mass run configurations
func DeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req api.DeleteMassRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	names := req.Names
	if req.Name != "" {
		names = append(names, req.Name)
	}

	// Validate required fields
	if len(names) == 0 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "names is required and must not be empty",
		})
		return
	}

	// Delete from database
	if err := database.DeleteMassRunConfigs(names); err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": fmt.Sprintf("Failed to delete mass run configuration: %v", err),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "success",
		"message":       "Mass run configuration(s) deleted successfully",
		"deleted_count": len(names),
	})
}

// ReplaceHandler handles requests to replace all mass run configurations
func ReplaceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req api.ReplaceMassRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if err := validateMassRunRequests(req.Configs); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	dbConfigs := make([]*database.MassRunConfig, len(req.Configs))
	for i, config := range req.Configs {
		envVars := config.EnvVars
		if envVars == nil {
			envVars = make(map[string]string)
		}

		dbConfigs[i] = &database.MassRunConfig{
			Name:          config.Name,
			ContainerName: config.ContainerName,
			Port:          config.Port,
			Label:         config.Label,
			EnvVars:       envVars,
		}
	}

	if err := database.ReplaceMassRunConfigs(dbConfigs); err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": fmt.Sprintf("Failed to replace mass run configurations: %v", err),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, api.ReplaceMassRunResponse{
		Status:        "success",
		Message:       "Mass run configurations replaced successfully",
		ReplacedCount: len(req.Configs),
	})
}

// ExecuteHandler starts a container for every saved mass-run preset using the given image.
func ExecuteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req api.ExecuteMassRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if strings.TrimSpace(req.ImageName) == "" {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "image_name is required",
		})
		return
	}

	configs, err := database.ListMassRunConfigs()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": fmt.Sprintf("Failed to list mass run configurations: %v", err),
		})
		return
	}

	if len(configs) == 0 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "No mass run configurations found. Add some in the Mass Run tab first.",
		})
		return
	}

	started := 0
	failed := make([]api.MassRunExecuteFailure, 0)
	for _, config := range configs {
		runReq := api.RunContainerRequest{
			ImageName:     req.ImageName,
			ContainerName: config.ContainerName,
			Port:          config.Port,
			Label:         config.Label,
			EnvVars:       config.EnvVars,
		}
		if len(runReq.EnvVars) == 0 {
			runReq.EnvVars = nil
		}

		if _, err := runFromRequest(runReq); err != nil {
			failed = append(failed, api.MassRunExecuteFailure{
				Name:   config.Name,
				Detail: err.Error(),
			})
			continue
		}
		started++
	}

	status := "success"
	if len(failed) > 0 {
		status = "partial"
	}

	utils.WriteJSON(w, http.StatusOK, api.ExecuteMassRunResponse{
		Status:  status,
		Started: started,
		Failed:  failed,
	})
}
