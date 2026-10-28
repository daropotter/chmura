package manifest

import (
	"fmt"
	"maps"
	"slices"
)

// Port names an application listener and the scope in which it is reachable.
type Port struct {
	Number     int    `yaml:"number"`
	Protocol   string `yaml:"protocol"`
	Visibility string `yaml:"visibility,omitempty"`
}

func (a Application) normalizePorts(path string) error {
	for _, name := range slices.Sorted(maps.Keys(a.Ports)) {
		field := path + ".ports." + name
		port := a.Ports[name]
		if port.Number < 1 || port.Number > 65535 {
			return fmt.Errorf("%s.number: expected a port between 1 and 65535", field)
		}
		if !slices.Contains([]string{"http", "https", "tcp", "udp"}, port.Protocol) {
			return fmt.Errorf("%s.protocol: expected http, https, tcp or udp", field)
		}
		if port.Visibility == "" {
			port.Visibility = "project"
		}
		if !slices.Contains([]string{"application", "project", "space", "public"}, port.Visibility) {
			return fmt.Errorf("%s.visibility: expected application, project, space or public", field)
		}
		a.Ports[name] = port
	}
	return nil
}
