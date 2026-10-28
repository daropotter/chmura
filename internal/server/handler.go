// Package server implements the Chmura control-plane API.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/daropotter/chmura/internal/api"
	"github.com/daropotter/chmura/internal/version"
)

// NewHandler builds the API for the installation's explicitly selected profile.
func NewHandler(profile string) (http.Handler, error) {
	if profile != "production" && profile != "dev" {
		return nil, fmt.Errorf("invalid installation profile %q: use production or dev", profile)
	}
	info := api.Info{
		ServerVersion: version.Version,
		APIVersions:   []string{api.Version},
		Profile:       profile,
		Features:      []string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})
	return mux, nil
}
