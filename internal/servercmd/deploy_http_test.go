package servercmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

const apiDeployManifest = "version: 1\nname: shop\napplications:\n  web: {source: {image: ghcr.io/acme/web:v1}}\n"

func TestCommandDeployRequiresBootstrapAdminOverHTTPS(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, tokenPath := filepath.Join(dir, "state.db"), filepath.Join(dir, "credential")
	token := writeBootstrapFixture(t, tokenPath)
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cluster, _, err := store.CreateCluster(ctx, state.CreateClusterRequest{Name: "home", IdempotencyKey: "cluster"})
	if err != nil {
		t.Fatal(err)
	}
	space, _, err := store.CreateSpace(ctx, state.CreateSpaceRequest{Name: "production", ClusterID: cluster.ID, IdempotencyKey: "space"})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, state.CreateProjectRequest{Name: "shop", SpaceID: space.ID, IdempotencyKey: "project"})
	if err != nil {
		t.Fatal(err)
	}
	client, endpoint := startDeployServer(t, path)
	url := endpoint + "/v1/spaces/" + space.ID + "/projects/" + project.ID + "/deployments"

	body, err := json.Marshal(map[string]any{"base-version": 0, "manifest": apiDeployManifest})
	if err != nil {
		t.Fatal(err)
	}
	bearer := "Bearer " + string(token)
	if resp, _ := postDeployment(t, client, url, bearer, "deploy", "application/json", body, http.StatusUnauthorized); resp.Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("uninitialized server omitted the bearer challenge")
	}
	var out, stderr bytes.Buffer
	if code := Execute(ctx, initArgs(path, tokenPath), &out, &stderr); code != 0 {
		t.Fatalf("bootstrap exited %d", code)
	}
	wrongToken := writeBootstrapFixture(t, filepath.Join(dir, "unregistered"))
	for _, tc := range []struct {
		name, auth, key, contentType string
		body                         []byte
		status                       int
	}{
		{"missing credential", "", "deploy", "application/json", body, http.StatusUnauthorized},
		{"wrong credential", "Bearer " + string(wrongToken), "deploy", "application/json", body, http.StatusUnauthorized},
		{"malformed credential", "Bearer invalid", "deploy", "application/json", body, http.StatusUnauthorized},
		{"authenticate before decode", "", "", "text/plain", []byte("{"), http.StatusUnauthorized},
		{"missing key", bearer, "", "application/json", body, http.StatusBadRequest},
		{"wrong content type", bearer, "deploy", "text/plain", body, http.StatusUnsupportedMediaType},
		{"missing base", bearer, "deploy", "application/json", []byte(`{"manifest":"x"}`), http.StatusBadRequest},
		{"invalid base", bearer, "deploy", "application/json", []byte(`{"base-version":1,"manifest":"x"}`), http.StatusBadRequest},
		{"trailing JSON", bearer, "deploy", "application/json", append(bytes.Clone(body), []byte(" {}")...), http.StatusBadRequest},
		{"unknown field", bearer, "deploy", "application/json", []byte(`{"base-version":0,"manifest":"x","extra":true}`), http.StatusBadRequest},
		{"invalid manifest", bearer, "deploy", "application/json", []byte(`{"base-version":0,"manifest":"version: ["}`), http.StatusBadRequest},
		{"oversize", bearer, "deploy", "application/json", bytes.Repeat([]byte(" "), 1<<20+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, response := postDeployment(t, client, url, tc.auth, tc.key, tc.contentType, tc.body, tc.status)
			if bytes.Contains(response, token) || bytes.Contains(response, wrongToken) {
				t.Fatal("response exposed a credential")
			}
		})
	}
	missing := endpoint + "/v1/spaces/another-space/projects/" + project.ID + "/deployments"
	postDeployment(t, client, missing, bearer, "deploy", "application/json", body, http.StatusNotFound)
	if _, err := store.CurrentRevision(ctx, space.ID, project.ID); err != state.ErrNotFound {
		t.Fatalf("rejected submissions changed the project: %v", err)
	}
	_, accepted := postDeployment(t, client, url, bearer, "deploy", "application/json", body, http.StatusAccepted)
	type receipt struct {
		OperationID string                `json:"operation-id"`
		Status      state.OperationStatus `json:"status"`
	}
	var result receipt
	if err := json.Unmarshal(accepted, &result); err != nil || result.OperationID == "" || result.Status != state.OperationPending {
		t.Fatalf("acceptance did not include the pending operation: %v", err)
	}
	head, err := store.CurrentRevision(ctx, space.ID, project.ID)
	if err != nil || head.OperationID != result.OperationID || head.Version != 1 {
		t.Fatalf("HTTP acceptance did not persist selection: %+v error=%v", head, err)
	}
	postDeployment(t, client, url, bearer, "stale", "application/json", body, http.StatusConflict)
	nextBody, err := json.Marshal(map[string]any{
		"base-version": head.Version, "base-revision-id": head.RevisionID, "manifest": apiDeployManifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	postDeployment(t, client, url, bearer, "next", "application/json", nextBody, http.StatusAccepted)
	nextHead, err := store.CurrentRevision(ctx, space.ID, project.ID)
	if err != nil || nextHead.Version != 2 || nextHead.BaseRevisionID != head.RevisionID {
		t.Fatalf("HTTP acceptance did not advance selection: %+v error=%v", nextHead, err)
	}
	_, replay := postDeployment(t, client, url, bearer, "deploy", "application/json", body, http.StatusAccepted)
	var repeated receipt
	if err := json.Unmarshal(replay, &repeated); err != nil || repeated != result {
		t.Fatal("exact retry did not return the original receipt")
	}
}

func TestCommandWithoutStateKeepsDiscoveryOnly(t *testing.T) {
	client, endpoint := startDeployServer(t, "")
	postDeployment(t, client, endpoint+"/v1/spaces/any/projects/any/deployments", "", "", "application/json", nil, http.StatusNotFound)
}

func startDeployServer(t *testing.T, statePath string) (*http.Client, string) {
	t.Helper()
	cert, key, client := commandTLSFixture(t)
	reader, writer := io.Pipe()
	serve, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	args := stateServeArgs(cert, key, statePath)
	if statePath == "" {
		args = args[:len(args)-2]
	}
	go func() {
		defer writer.Close()
		done <- Execute(serve, args, writer, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		_ = reader.Close()
		select {
		case code := <-done:
			if code != 130 {
				t.Errorf("server exited with code %d", code)
			}
		case <-time.After(6 * time.Second):
			t.Error("server did not stop")
		}
	})
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(reader).ReadString('\n')
		lines <- line
	}()
	select {
	case line := <-lines:
		for _, word := range strings.Fields(line) {
			if strings.HasPrefix(word, "https://") {
				return client, word
			}
		}
		t.Fatal("server did not announce an HTTPS endpoint")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not announce readiness")
	}
	return nil, ""
}

func postDeployment(t *testing.T, client *http.Client, url, auth, key, contentType string, body []byte, want int) (http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("HTTPS deploy status %d, want %d", res.StatusCode, want)
	}
	return res.Header, payload
}
