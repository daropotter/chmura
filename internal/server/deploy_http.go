package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"

	"github.com/daropotter/chmura/internal/api"
	"github.com/daropotter/chmura/internal/identity"
	"github.com/daropotter/chmura/internal/state"
)

// AdminDeployStore verifies the built-in administrator and accepts deploys.
type AdminDeployStore interface {
	DeployStore
	AuthenticateAdmin(context.Context, [32]byte) (state.Admin, error)
}

// RegisterDeployRoute adds the stateful deploy endpoint. The bootstrapped
// administrator has installation-wide access to existing project targets.
func RegisterDeployRoute(mux *http.ServeMux, store AdminDeployStore) {
	mux.HandleFunc("POST /v1/spaces/{space}/projects/{project}/deployments", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			writeDeployError(w, http.StatusBadRequest, "deploy requires HTTPS")
			return
		}
		if !authorizeDeploy(w, r, store) {
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || strings.TrimSpace(keys[0]) == "" {
			writeDeployError(w, http.StatusBadRequest, "one Idempotency-Key header is required")
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeDeployError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		request, err := decodeDeployRequest(w, r)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeDeployError(w, http.StatusRequestEntityTooLarge, "deploy request is too large")
			} else {
				writeDeployError(w, http.StatusBadRequest, "expected one JSON deploy request")
			}
			return
		}
		if !validDeployRequest(request) {
			writeDeployError(w, http.StatusBadRequest, "deploy requires a manifest and a valid base version/revision pair")
			return
		}
		accepted, _, err := SubmitDeploy(r.Context(), store, DeploySubmission{
			SpaceID: r.PathValue("space"), ProjectID: r.PathValue("project"), IdempotencyKey: keys[0],
			ExpectedVersion: *request.BaseVersion, ExpectedRevisionID: request.BaseRevisionID, Manifest: []byte(request.Manifest),
		})
		switch {
		case errors.Is(err, ErrInvalidDeployManifest):
			writeDeployError(w, http.StatusBadRequest, "invalid deploy manifest")
		case errors.Is(err, state.ErrNotFound):
			writeDeployError(w, http.StatusNotFound, "project not found in space")
		case errors.Is(err, state.ErrConflict):
			writeDeployError(w, http.StatusConflict, "deploy conflicts with project state or an existing request")
		case err != nil:
			writeDeployError(w, http.StatusInternalServerError, "could not accept deploy")
		default:
			writeDeployJSON(w, http.StatusAccepted, api.DeployAccepted{OperationID: accepted.Operation.ID, Status: string(accepted.Operation.Status)})
		}
	})
}

func authorizeDeploy(w http.ResponseWriter, r *http.Request, store AdminDeployStore) bool {
	if headers := r.Header.Values("Authorization"); len(headers) == 1 {
		scheme, token, _ := strings.Cut(headers[0], " ")
		if strings.EqualFold(scheme, "Bearer") {
			if digest, err := identity.AdminTokenDigest([]byte(token)); err == nil {
				_, err := store.AuthenticateAdmin(r.Context(), digest)
				if err == nil {
					return true
				}
				if !errors.Is(err, state.ErrNotFound) {
					writeDeployError(w, http.StatusInternalServerError, "could not authenticate deploy")
					return false
				}
			}
		}
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeDeployError(w, http.StatusUnauthorized, "valid administrator bearer token required")
	return false
}

const maxDeployBodyBytes = 1 << 20

func decodeDeployRequest(w http.ResponseWriter, r *http.Request) (api.DeployRequest, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeployBodyBytes))
	decoder.DisallowUnknownFields()
	var request api.DeployRequest
	if err := decoder.Decode(&request); err != nil {
		return api.DeployRequest{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("additional JSON value")
		}
		return api.DeployRequest{}, err
	}
	return request, nil
}

func validDeployRequest(request api.DeployRequest) bool {
	if request.BaseVersion == nil || strings.TrimSpace(request.Manifest) == "" {
		return false
	}
	if *request.BaseVersion == 0 {
		return request.BaseRevisionID == ""
	}
	return *request.BaseVersion > 0 && *request.BaseVersion < math.MaxInt64 && strings.TrimSpace(request.BaseRevisionID) != ""
}

func writeDeployError(w http.ResponseWriter, status int, message string) {
	writeDeployJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

func writeDeployJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
