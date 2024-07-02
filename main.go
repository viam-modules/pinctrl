//go:build linux

package main

import (
	"context"
	"fmt"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
	"go.viam.com/utils"

	pi5 "pinctrl/pi5"

	"go.viam.com/rdk/components/board"
)

func main() {
	utils.ContextualMain(mainWithArgs, module.NewLoggerFromArgs("pinctrl"))
}

func mainWithArgs(ctx context.Context, args []string, logger logging.Logger) error {
	fmt.Printf("before new module from argg\n")
	pinctrl, err := module.NewModuleFromArgs(ctx, logger)
	fmt.Printf("new module from argg\n")

	if err != nil {
		return err
	}

	fmt.Printf("before add mod from reg\n")
	if err = pinctrl.AddModelFromRegistry(ctx, board.API, pi5.Model); err != nil {
		return err
	}
	fmt.Printf("add mod from reg\n")

	err = pinctrl.Start(ctx)
	fmt.Printf("start done\n")

	defer pinctrl.Close(ctx)
	fmt.Printf("close\n")
	if err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}
