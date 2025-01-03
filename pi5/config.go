// Package pi5 defines pi5
package pi5

import (
	"errors"

	"go.viam.com/rdk/resource"
)

// Config defines config.
type Config struct {
	Pulls []PullConfig `json:"pulls,omitempty"`
}

// PullConfig defines the config for pull up/pull down resistors.
type PullConfig struct {
	Pin  string `json:"pin"`
	Pull string `json:"pull"`
}

// Validate validates the config.
func (cfg *Config) Validate(path string) ([]string, error) {
	for _, c := range cfg.Pulls {
		if c.Pin == "" {
			return []string{}, resource.NewConfigValidationFieldRequiredError(path, "pin")
		}
		if c.Pull == "" {
			return []string{}, resource.NewConfigValidationFieldRequiredError(path, "pull")
		}
		if !(c.Pull == "up" || c.Pull == "down" || c.Pull == "none") {
			return []string{}, errors.New("supported pull config attributes are up, down, and none")
		}
	}
	return []string{}, nil
}
