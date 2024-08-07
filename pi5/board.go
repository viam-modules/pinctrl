//go:build linux

package pi5

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	mmap "github.com/edsrzf/mmap-go"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	pb "go.viam.com/api/component/board/v1"
	"go.viam.com/utils"

	"go.viam.com/rdk/components/board"
	gl "go.viam.com/rdk/components/board/genericlinux"
	"go.viam.com/rdk/grpc"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Model for rpi5.
var Model = resource.NewModel("viam-labs", "pinctrl", "rpi5")

func init() {
	gpioMappings, err := gl.GetGPIOBoardMappings(Model.Name, boardInfoMappings)
	var noBoardErr gl.NoBoardFoundError
	if errors.As(err, &noBoardErr) {
		logging.Global().Debugw("Error getting raspi5 GPIO board mapping", "error", err)
	}

	RegisterBoard(Model.Name, gpioMappings)
}

// RegisterBoard registers a sysfs based board of the given model.
// using this constructor to pass in the GPIO mappings.
func RegisterBoard(modelName string, gpioMappings map[string]gl.GPIOBoardMapping) {
	resource.RegisterComponent(
		board.API,
		Model,
		resource.Registration[board.Board, *Config]{
			Constructor: func(
				ctx context.Context,
				_ resource.Dependencies,
				conf resource.Config,
				logger logging.Logger,
			) (board.Board, error) {
				return newBoard(ctx, conf, ConstPinDefs(gpioMappings), logger, false)
			},
		})
}

// newBoard is the constructor for a Board.
func newBoard(
	ctx context.Context,
	conf resource.Config,
	convertConfig ConfigConverter,
	logger logging.Logger,
	testingMode bool,
) (board.Board, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	b := &pinctrlpi5{
		Named:         conf.ResourceName().AsNamed(),
		convertConfig: convertConfig,

		logger:     logger,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,

		gpios:      map[string]*gpioPin{},
		interrupts: map[string]*digitalInterrupt{},

		// store addresses + other stuff here
		chipSize: 0x30000,
	}
	if err := b.Reconfigure(ctx, nil, conf); err != nil {
		return nil, err
	}

	if err := b.setupPinControl(testingMode); err != nil {
		return nil, err
	}
	return b, nil
}

// Reconfigure reconfigures the board.
func (b *pinctrlpi5) Reconfigure(
	ctx context.Context,
	_ resource.Dependencies,
	conf resource.Config,
) error {
	newConf, err := b.convertConfig(conf, b.logger)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.reconfigureGpios(newConf); err != nil {
		return err
	}
	return nil
}

// This is a helper function used to reconfigure the GPIO pins. It looks for the key in the map
// whose value resembles the target pin definition.
func getMatchingPin(target gl.GPIOBoardMapping, mapping map[string]gl.GPIOBoardMapping) (string, bool) {
	for name, def := range mapping {
		if target == def {
			return name, true
		}
	}
	return "", false
}

func (b *pinctrlpi5) reconfigureGpios(newConf *LinuxBoardConfig) error {
	// First, find old pins that are no longer defined, and destroy them.
	for oldName, mapping := range b.gpioMappings {
		if _, ok := getMatchingPin(mapping, newConf.GpioMappings); ok {
			continue // This pin is in the new mapping, so don't destroy it.
		}

		// Otherwise, remove the pin because it's not in the new mapping.
		if pin, ok := b.gpios[oldName]; ok {
			if err := pin.Close(); err != nil {
				return err
			}
			delete(b.gpios, oldName)
			continue
		}

		// If we get here, the old pin definition exists, but the old pin does not. Check if it's a
		// digital interrupt.
		if interrupt, ok := b.interrupts[oldName]; ok {
			if err := interrupt.Close(); err != nil {
				return err
			}
			delete(b.interrupts, oldName)
			continue
		}

		// If we get here, there is a logic bug somewhere. but failing to delete a nonexistent pin
		// seemingly doesn't hurt anything, so just log the error and continue.
		b.logger.Errorf("During reconfiguration, old pin '%s' should be destroyed, but "+
			"it doesn't exist!?", oldName)
	}

	// Next, compare the new pin definitions to the old ones, to build up 2 sets: pins to rename,
	// and new pins to create. Don't actually create any yet, in case you'd overwrite a pin that
	// should be renamed out of the way first.
	toRename := map[string]string{} // Maps old names for pins to new names
	toCreate := map[string]gl.GPIOBoardMapping{}
	for newName, mapping := range newConf.GpioMappings {
		if oldName, ok := getMatchingPin(mapping, b.gpioMappings); ok {
			if oldName != newName {
				toRename[oldName] = newName
			}
		} else {
			toCreate[newName] = mapping
		}
	}

	// Rename the ones whose name changed. The ordering here is tricky: if B should be renamed to C
	// while A should be renamed to B, we need to make sure we don't overwrite B with A and then
	// rename it to C. To avoid this, move all the pins to rename into a temporary data structure,
	// then move them all back again afterward.
	tempGpios := map[string]*gpioPin{}
	tempInterrupts := map[string]*digitalInterrupt{}
	for oldName, newName := range toRename {
		if pin, ok := b.gpios[oldName]; ok {
			tempGpios[newName] = pin
			delete(b.gpios, oldName)
			continue
		}

		// If we get here, again check if the missing pin is a digital interrupt.
		if interrupt, ok := b.interrupts[oldName]; ok {
			tempInterrupts[newName] = interrupt
			delete(b.interrupts, oldName)
			continue
		}

		return fmt.Errorf("during reconfiguration, old pin '%s' should be renamed to '%s', but "+
			"it doesn't exist!?", oldName, newName)
	}

	// Now move all the pins back from the temporary data structures.
	for newName, pin := range tempGpios {
		b.gpios[newName] = pin
	}
	for newName, interrupt := range tempInterrupts {
		b.interrupts[newName] = interrupt
	}

	// Finally, create the new pins.
	for newName, mapping := range toCreate {
		b.gpios[newName] = b.createGpioPin(mapping)
	}

	b.gpioMappings = newConf.GpioMappings
	return nil
}

func (b *pinctrlpi5) createGpioPin(mapping gl.GPIOBoardMapping) *gpioPin {
	pin := gpioPin{
		boardWorkers: &b.activeBackgroundWorkers,
		devicePath:   mapping.GPIOChipDev,
		offset:       uint32(mapping.GPIO),
		cancelCtx:    b.cancelCtx,
		logger:       b.logger,
	}
	if mapping.HWPWMSupported {
		pin.hwPwm = newPwmDevice(mapping.PWMSysFsDir, mapping.PWMID, b.logger, &b.vPage)
	}
	return &pin
}

// Board implements a component for a Linux machine.
type pinctrlpi5 struct {
	resource.Named
	mu            sync.RWMutex
	convertConfig ConfigConverter

	gpioMappings map[string]gl.GPIOBoardMapping
	logger       logging.Logger

	gpios      map[string]*gpioPin
	interrupts map[string]*digitalInterrupt

	virtAddr *byte     // base address of mapped virtual page referencing the gpio chip data
	physAddr uint64    // base address of the gpio chip data in /dev/mem/
	chipSize uint64    // length of chip's address space in memory
	memFile  *os.File  // actual file to open that the virtual page will point to. Need to keep track of this for cleanup
	vPage    mmap.MMap // virtual page pointing to dev/gpiomem's physical page in memory. Need to keep track of this for cleanup

	cancelCtx               context.Context
	cancelFunc              func()
	activeBackgroundWorkers sync.WaitGroup
}

// AnalogByName returns the analog pin by the given name if it exists.
func (b *pinctrlpi5) AnalogByName(name string) (board.Analog, error) {
	return nil, errors.New("analogs not supported")
}

// DigitalInterruptByName returns the interrupt by the given name if it exists.
func (b *pinctrlpi5) DigitalInterruptByName(name string) (board.DigitalInterrupt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	interrupt, ok := b.interrupts[name]
	if ok {
		return interrupt, nil
	}

	// Otherwise, the name is not something we recognize yet. If it appears to be a GPIO pin, we'll
	// remove its GPIO capabilities and turn it into a digital interrupt.
	gpio, ok := b.gpios[name]
	if !ok {
		return nil, fmt.Errorf("can't find GPIO (%s)", name)
	}
	if err := gpio.Close(); err != nil {
		return nil, err
	}

	mapping, ok := b.gpioMappings[name]
	if !ok {
		return nil, fmt.Errorf("can't create digital interrupt on unknown pin %s", name)
	}
	defaultInterruptConfig := board.DigitalInterruptConfig{
		Name: name,
		Pin:  name,
	}
	interrupt, err := newDigitalInterrupt(defaultInterruptConfig, mapping, nil)
	if err != nil {
		return nil, err
	}

	delete(b.gpios, name)
	b.interrupts[name] = interrupt
	return interrupt, nil
}

// AnalogNames returns the names of all known analog pins.
func (b *pinctrlpi5) AnalogNames() []string {
	return []string{}
}

// DigitalInterruptNames returns the names of all known digital interrupts.
func (b *pinctrlpi5) DigitalInterruptNames() []string {
	if b.interrupts == nil {
		return nil
	}

	names := []string{}
	for name := range b.interrupts {
		names = append(names, name)
	}
	return names
}

// GPIOPinByName returns a GPIOPin by name.
func (b *pinctrlpi5) GPIOPinByName(pinName string) (board.GPIOPin, error) {
	if pin, ok := b.gpios[pinName]; ok {
		return pin, nil
	}

	// Check if pin is a digital interrupt: those can still be used as inputs.
	if interrupt, interruptOk := b.interrupts[pinName]; interruptOk {
		return interrupt, nil
	}

	return nil, errors.Errorf("cannot find GPIO for unknown pin: %s", pinName)
}

// SetPowerMode sets the board to the given power mode. If provided,
// the board will exit the given power mode after the specified
// duration.
func (b *pinctrlpi5) SetPowerMode(
	ctx context.Context,
	mode pb.PowerMode,
	duration *time.Duration,
) error {
	return grpc.UnimplementedError
}

// StreamTicks starts a stream of digital interrupt ticks.
func (b *pinctrlpi5) StreamTicks(ctx context.Context, interrupts []board.DigitalInterrupt, ch chan board.Tick,
	extra map[string]interface{},
) error {
	var rawInterrupts []*digitalInterrupt
	for _, i := range interrupts {
		raw, ok := i.(*digitalInterrupt)
		if !ok {
			return errors.New("cannot stream ticks to an interrupt not associated with this board")
		}
		rawInterrupts = append(rawInterrupts, raw)
	}

	for _, i := range rawInterrupts {
		i.AddChannel(ch)
	}

	b.activeBackgroundWorkers.Add(1)
	utils.ManagedGo(func() {
		// Wait until it's time to shut down then remove callbacks.
		select {
		case <-ctx.Done():
		case <-b.cancelCtx.Done():
		}
		for _, i := range rawInterrupts {
			i.RemoveChannel(ch)
		}
	}, b.activeBackgroundWorkers.Done)

	return nil
}

// Close attempts to cleanly close each part of the board.
func (b *pinctrlpi5) Close(ctx context.Context) error {
	b.mu.Lock()
	err := b.cleanupPinControl()
	if err != nil {
		return fmt.Errorf("trouble cleaning up pincontrol memory: %w", err)
	}
	b.cancelFunc()
	b.mu.Unlock()
	b.activeBackgroundWorkers.Wait()

	for _, pin := range b.gpios {
		err = multierr.Combine(err, pin.Close())
	}
	for _, interrupt := range b.interrupts {
		err = multierr.Combine(err, interrupt.Close())
	}
	return err
}
