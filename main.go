//go:build linux

package main

import (
	"context"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
	"go.viam.com/utils"

	pi5 "pinctrl/rpi5"

	"go.viam.com/rdk/components/board"
)

func main() {
	utils.ContextualMain(mainWithArgs, module.NewLoggerFromArgs("pinctrl"))
}

func mainWithArgs(ctx context.Context, args []string, logger logging.Logger) error {
	pinctrl, err := module.NewModuleFromArgs(ctx, logger)
	if err != nil {
		return err
	}
	if err = pinctrl.AddModelFromRegistry(ctx, board.API, pi5.Model); err != nil {
		return err
	}

	err = pinctrl.Start(ctx)

	defer pinctrl.Close(ctx)
	if err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}
