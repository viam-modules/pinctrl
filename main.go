//go:build linux

// package main
package main

import (
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"

	"github.com/viam-modules/pinctrl/pi5"
)

func main() {
	module.ModularMain(resource.APIModel{board.API, pi5.Model})
}
