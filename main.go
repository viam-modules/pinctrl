//go:build linux

// package main
package main

import (
	"github.com/viam-modules/pinctrl/pi5"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
)

func main() {
	module.ModularMain(resource.APIModel{board.API, pi5.Model})
}
