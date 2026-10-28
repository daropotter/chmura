// Package api defines the JSON contracts shared by control-plane clients and servers.
package api

// Version is the public API version implemented by this build.
const Version = "v1"

// Info identifies an installation and the API features it actually supports.
type Info struct {
	ServerVersion string   `json:"server-version"`
	APIVersions   []string `json:"api-versions"`
	Profile       string   `json:"profile"`
	Features      []string `json:"features"`
}
