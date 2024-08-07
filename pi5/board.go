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

// pins are stored in /dev/gpiomem in order of gpio nums, so we must convert from pin name (physical num) to GPIO number.
var pinNameToGPIONum = map[string]int{
	"3":  2,
	"5":  3,
	"7":  4,
	"8":  14,
	"10": 15,
	"11": 17,
	"12": 18,
	"13": 27,
	"15": 22,
	"16": 23,
	"18": 24,
	"19": 10,
	"21": 9,
	"22": 25,
	"23": 11,
	"24": 8,
	"26": 7,
	"27": 0,
	"28": 1,
	"29": 5,
	"31": 6,
	"32": 12,
	"33": 13,
	"35": 19,
	"36": 16,
	"37": 26,
	"38": 20,
	"40": 21,
}

// register values for configuring pull up/pull down in mem.
const (
	pullNoneMode = 0x0
	pullDownMode = 0x4
	pullUpMode   = 0x8
)

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
				return newBoard(ctx, conf, gpioMappings, logger, false)
			},
		})
}

// newBoard is the constructor for a Board.
func newBoard(
	ctx context.Context,
	conf resource.Config,
	gpioMappings map[string]gl.GPIOBoardMapping,
	logger logging.Logger,
	testingMode bool,
) (board.Board, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	b := &pinctrlpi5{
		Named:         conf.ResourceName().AsNamed(),

		logger:     logger,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,

		gpios:      map[string]*gpioPin{},
		interrupts: map[string]*digitalInterrupt{},

		chipSize: 0x30000,
		pulls:    map[int]byte{},
	}

	// Note that this must be called before reconfiguring the pull up/down configuration uses the
	// memory mapped in this function.
	if err := b.setupPinControl(testingMode); err != nil {
		return nil, err
	}

	if err := b.initializeGPIOs(gpioMappings); err != nil {
		return nil, err
	}

	newConf, err := b.convertConfig(conf, b.logger)
	if err != nil {
		return nil, err
	}
	if err := b.reconfigurePullUpPullDowns(newConf); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *pinctrlpi5) reconfigurePullUpPullDowns(newConf *LinuxBoardConfig) error {
	for _, pullConf := range newConf.Pulls {
		gpioNum := pinNameToGPIONum[pullConf.Pin]
		switch pullConf.Pull {
		case "none":
			b.pulls[gpioNum] = pullNoneMode
		case "up":
			b.pulls[gpioNum] = pullUpMode
		case "down":
			b.pulls[gpioNum] = pullDownMode
		default:
			return fmt.Errorf("unexpected pull")
		}

		b.setPulls()
	}

	return nil
}

// setPull is a helper function to access memory to set a pull up/pull down resisitor on a pin.
func (b *pinctrlpi5) setPulls() {
	// offset to the pads address space in /dev/gpiomem0
	// all gpio pins are in bank0
	PadsBank0Offset := 0x00020000

	for pin, mode := range b.pulls {
		// each pad has 4 header bytes + 4 bytes of memory for each gpio pin
		pinOffsetBytes := 4 + 4*pin

		// only the 5th and 6th bits of the register are used to set pull up/down
		// reset the register then set the mode
		b.vPage[PadsBank0Offset+pinOffsetBytes] = (b.vPage[PadsBank0Offset+pinOffsetBytes] & 0xf3) | mode
	}
}

func (b *pinctrlpi5) initializeGPIOs(gpioMappings map[string]gl.GPIOBoardMapping) error {
	for newName, mapping := range gpioMappings {
		b.gpios[newName] = b.createGpioPin(mapping)
	}
	b.gpioMappings = gpioMappings

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

type pinctrlpi5 struct {
	resource.Named
	resource.TriviallyReconfigurable
	mu            sync.RWMutex

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

	pulls map[int]byte // mapping of gpio pin to pull up/down
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
