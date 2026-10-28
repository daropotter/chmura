package manifest_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daropotter/chmura/internal/manifest"
)

func TestParseNamedPorts(t *testing.T) {
	for _, protocol := range []string{"http", "https", "tcp", "udp"} {
		for _, visibility := range []string{"application", "project", "space", "public"} {
			t.Run(protocol+"/"+visibility, func(t *testing.T) {
				input := imageProject + fmt.Sprintf("    ports:\n      service: {number: 8080, protocol: %s, visibility: %s}\n", protocol, visibility)
				project, err := manifest.Parse([]byte(input))
				if err != nil {
					t.Fatal(err)
				}
				port := project.Applications["api"].Ports["service"]
				if port.Number != 8080 || port.Protocol != protocol || port.Visibility != visibility {
					t.Errorf("port = %#v", port)
				}
			})
		}
	}
}

func TestParsePortDefaultsAndBoundaries(t *testing.T) {
	project, err := manifest.Parse([]byte(imageProject + `    ports:
      first: {number: 1, protocol: tcp}
      last: {number: 65535, protocol: udp}
`))
	if err != nil {
		t.Fatal(err)
	}
	ports := project.Applications["api"].Ports
	if len(ports) != 2 || ports["first"].Number != 1 || ports["last"].Number != 65535 {
		t.Fatalf("ports = %#v", ports)
	}
	for name, port := range ports {
		if port.Visibility != "project" {
			t.Errorf("%s visibility = %q, want project", name, port.Visibility)
		}
	}
}

func TestParsePortsOnDockerfileApplication(t *testing.T) {
	input := strings.Replace(imageProject, "image: ghcr.io/acme/api:1.0", "context: .\n      dockerfile: Dockerfile", 1)
	project, err := manifest.Parse([]byte(input + "    ports: {http: {number: 8080, protocol: http}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if project.Applications["api"].Ports["http"].Visibility != "project" {
		t.Fatal("port default was not applied to a Dockerfile application")
	}
}

func TestParseRejectsInvalidPorts(t *testing.T) {
	for _, tc := range []struct{ name, ports, signal string }{
		{"missing number", "http: {protocol: http}", ".ports.http.number"},
		{"zero", "http: {number: 0, protocol: http}", ".ports.http.number"},
		{"negative", "http: {number: -1, protocol: http}", ".ports.http.number"},
		{"too large", "http: {number: 65536, protocol: http}", ".ports.http.number"},
		{"missing protocol", "http: {number: 8080}", ".ports.http.protocol"},
		{"unknown protocol", "http: {number: 8080, protocol: sctp}", ".ports.http.protocol"},
		{"protocol casing", "http: {number: 8080, protocol: HTTP}", ".ports.http.protocol"},
		{"unknown visibility", "http: {number: 8080, protocol: http, visibility: private}", ".ports.http.visibility"},
		{"null port", "http: null", ".ports.http.number"},
		{"typo", "http: {number: 8080, protcol: http}", "protcol"},
		{"duplicate", "http: {number: 8080, protocol: http}, http: {number: 9000, protocol: http}", "http"},
		{"quoted number", `http: {number: "8080", protocol: http}`, "integer"},
		{"fractional number", "http: {number: 8080.5, protocol: http}", "integer"},
		{"boolean protocol", "http: {number: 8080, protocol: true}", "string"},
		{"empty name", `"": {number: 8080, protocol: http}`, "string"},
		{"numeric name", "123: {number: 8080, protocol: http}", "string"},
		{"empty visibility", `http: {number: 8080, protocol: http, visibility: ""}`, "string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, err := manifest.Parse([]byte(imageProject + "    ports: {" + tc.ports + "}\n"))
			if err == nil || project != nil {
				t.Fatalf("invalid port accepted: project=%#v error=%v", project, err)
			}
			if !strings.Contains(err.Error(), tc.signal) {
				t.Errorf("error = %q, want signal %q", err, tc.signal)
			}
		})
	}
}
