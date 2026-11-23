package utils

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes a JSON response with the given status code and data
func WriteJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// SuccessResponse sends a success JSON response
func SuccessResponse(w http.ResponseWriter, data map[string]interface{}) {
	if data == nil {
		data = make(map[string]interface{})
	}
	data["status"] = "success"
	WriteJSON(w, http.StatusOK, data)
}

// ErrorResponse sends an error JSON response
func ErrorResponse(w http.ResponseWriter, message string, statusCode int) {
	WriteJSON(w, statusCode, map[string]interface{}{
		"status": "error",
		"detail": message,
	})
}
