// Package pi5 defines pi5
package pi5

import (
	"fmt"

	"go.viam.com/rdk/components/board/genericlinux"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Config defines config.
type Config struct {
	Pulls []PullConfig `json:"pull,omitempty"`
}

// PullConfig defines the config for pull up/pull down resistors.
type PullConfig struct {
	Pin  string `json:"pin"`
	Pull string `json:"pulls"`
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
			return []string{}, fmt.Errorf("supported pull config attributes are up, down, and none")
		}
	}
	return []string{}, nil
}
