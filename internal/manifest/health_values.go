package manifest

import (
	"errors"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Duration is a positive duration when specified in YAML; zero means omitted.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(data []byte) error {
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return err
	}
	text, ok := value.(string)
	if !ok {
		return errors.New("expected a positive duration string, such as 5s or 2m")
	}
	parsed, err := time.ParseDuration(text)
	if err != nil || parsed <= 0 {
		return errors.New("expected a positive duration, such as 5s or 2m")
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Threshold counts consecutive check outcomes and must be positive when specified.
type Threshold int

func (c *Threshold) UnmarshalYAML(data []byte) error {
	if err := decodeInteger((*int)(c), data); err != nil {
		return err
	}
	if *c <= 0 {
		return errors.New("check thresholds must be positive integers")
	}
	return nil
}

// Command preserves explicit arguments, including empty arguments after the executable.
type Command []string

func (c *Command) UnmarshalYAML(data []byte) error {
	var values []any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return err
	}
	if len(values) == 0 {
		return errors.New("command requires a non-empty executable")
	}
	command := make(Command, len(values))
	for i, value := range values {
		text, ok := value.(string)
		if !ok || (i == 0 && strings.TrimSpace(text) == "") {
			return errors.New("command arguments must be strings, with a non-empty executable")
		}
		command[i] = text
	}
	*c = command
	return nil
}
