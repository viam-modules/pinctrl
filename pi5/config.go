// Package pi5 defines pi5
package pi5

import (
	"fmt"

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
			return []string{}, fmt.Errorf("supported pull config attributes are up, down, and none")
		}
	}
	return []string{}, nil
}

// LinuxBoardConfig is a struct containing absolutely everything a genericlinux board might need
// configured. It is a union of the configs for the customlinux boards and the genericlinux boards
// with static pin definitions, because those components all use the same underlying code but have
// different config types (e.g., only customlinux can change its pin definitions during
// reconfiguration). The LinuxBoardConfig struct is a unification of the two of them. Whenever we
// go through reconfiguration, we convert the provided config into a LinuxBoardConfig, and then
// reconfigure based on it.
type LinuxBoardConfig struct {
	Pulls        []PullConfig
	GpioMappings map[string]GPIOBoardMapping
}

// ConfigConverter is a type synonym for a function to turn whatever config we get during
// reconfiguration into a LinuxBoardConfig, so that we can reconfigure based on that. We return a
// pointer to a LinuxBoardConfig instead of the struct itself so that we can return nil if we
// encounter an error.
type ConfigConverter = func(resource.Config, logging.Logger) (*LinuxBoardConfig, error)

// ConstPinDefs takes in a map from pin names to GPIOBoardMapping structs, and returns a
// ConfigConverter that will use these pin definitions in the underlying config. It is intended to
// be used for board components whose pin definitions are built into the RDK, such as the
// BeagleBone or Jetson boards.
func ConstPinDefs(gpioMappings map[string]GPIOBoardMapping) ConfigConverter {
	return func(conf resource.Config, logger logging.Logger) (*LinuxBoardConfig, error) {
		newConf, err := resource.NativeConfig[*Config](conf)
		if err != nil {
			return nil, err
		}

		return &LinuxBoardConfig{
			GpioMappings: gpioMappings,
			Pulls:        newConf.Pulls,
		}, nil
	}
}
