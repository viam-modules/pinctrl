//go:build linux

package pinctrl

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/mkch/gpio"
	"go.uber.org/multierr"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/board/genericlinux"
	"go.viam.com/utils"
)

// DigitalInterrupt is the struct for managing a digital interrupt that satisfies a board.DigitalInterrupt.
type DigitalInterrupt struct {
	workers             *utils.StoppableWorkers
	line                *gpio.LineWithEvent
	mu                  sync.Mutex // Protects everything below here
	config              board.DigitalInterruptConfig
	count               int64
	channels            []chan board.Tick
	lastEvent           int64 // last time the event was triggered
	debounceNanoSeconds int64
}

// NewDigitalInterrupt constructs a new digitalInterrupt from the config and pinMapping. If
// oldInterrupt is not nil, all channels added to it are added to the new interrupt and removed
// from the old one.
// debounceMilliSeconds allows users to set a software debounce timer for their interrupt.
// Setting this to 0 disables the feature.
func (ctrl *Pinctrl) NewDigitalInterrupt(
	config board.DigitalInterruptConfig,
	pinMapping genericlinux.GPIOBoardMapping,
	debounceMilliSeconds int,
	oldInterrupt *DigitalInterrupt,
) (*DigitalInterrupt, error) {
	chip, err := gpio.OpenChip(pinMapping.GPIOChipDev)
	if err != nil {
		return nil, err
	}
	defer utils.UncheckedErrorFunc(chip.Close)

	line, err := chip.OpenLineWithEvents(
		uint32(pinMapping.GPIO), gpio.Input, gpio.BothEdges, "viam-interrupt")
	if err != nil {
		return nil, err
	}

	di := DigitalInterrupt{line: line, config: config, debounceNanoSeconds: int64(debounceMilliSeconds) * 1000000}
	di.workers = utils.NewBackgroundStoppableWorkers(di.monitor)

	if oldInterrupt != nil {
		oldInterrupt.mu.Lock()
		defer oldInterrupt.mu.Unlock()
		di.channels = oldInterrupt.channels
		oldInterrupt.channels = []chan board.Tick{}
	}

	return &di, nil
}

// UpdateConfig updates the config for the interrupt.
func (di *DigitalInterrupt) UpdateConfig(newConfig board.DigitalInterruptConfig) {
	di.mu.Lock()
	defer di.mu.Unlock()
	di.config = newConfig
}

// Close closes the interrupt.
func (di *DigitalInterrupt) Close() error {
	di.workers.Stop()

	// The background worker has now stopped, so cannot have locked the mutex. Anything else cannot
	// lock the mutex for very long, so we'll be able to acquire it quickly here.
	di.mu.Lock()
	defer di.mu.Unlock()
	var err error
	if len(di.channels) > 0 {
		err = fmt.Errorf("closed digital interrupt %s, but it still had %d listeners",
			di.config.Name, len(di.channels))
	}
	return multierr.Combine(err, di.line.Close())
}

// Name returns the name of the interrupt.
func (di *DigitalInterrupt) Name() string {
	di.mu.Lock()
	defer di.mu.Unlock()
	return di.config.Name
}

// Value gets the current count of interrupt triggers.
func (di *DigitalInterrupt) Value(
	ctx context.Context,
	extra map[string]interface{},
) (int64, error) {
	di.mu.Lock()
	defer di.mu.Unlock()
	return di.count, nil
}

func (di *DigitalInterrupt) monitor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-di.line.Events():
			// Put the body of this case in an anonymous function so we unlock the mutex when it's
			// finished.
			shouldReturn := func() bool {
				di.mu.Lock()
				defer di.mu.Unlock()
				eventTime := event.Time.UnixNano()
				// check if we should update
				if di.debounceNanoSeconds != 0 && eventTime-di.lastEvent < di.debounceNanoSeconds {
					// we have not passed the debounce time, ignore this interrupt
					return false
				}

				if event.RisingEdge {
					di.count++
				}

				tick := board.Tick{
					Name:             di.config.Name,
					High:             event.RisingEdge,
					TimestampNanosec: uint64(eventTime),
				}

				//  store the timestamp of the interrupt trigger
				di.lastEvent = eventTime
				for _, ch := range di.channels {
					select {
					case <-ctx.Done():
						return true // Stop the entire monitor
					case ch <- tick:
					}
				}
				return false
			}() // Execute the anonymous function, then unlock the mutex again
			if shouldReturn {
				return
			}
		}
	}
}

// AddChannel adds a board.Tick channel to stream ticks to.
func (di *DigitalInterrupt) AddChannel(ch chan board.Tick) {
	di.mu.Lock()
	defer di.mu.Unlock()
	di.channels = append(di.channels, ch)
}

// RemoveChannel removes a previously-added channel to stream ticks to.
func (di *DigitalInterrupt) RemoveChannel(ch chan board.Tick) {
	di.mu.Lock()
	defer di.mu.Unlock()
	for i, oldCh := range di.channels {
		if ch != oldCh {
			continue
		}

		// To remove this item, move the last item in the list to here and then truncate the list.
		lastIndex := len(di.channels) - 1
		di.channels[i] = di.channels[lastIndex]
		di.channels = di.channels[:lastIndex]
		break
	}
}

// You can also get the current value from a digitalInterrupt pin. To do this, we provide the
// entire board.GPIOPin interface.

// Set is an unimplemented placeholder for GPIOPin.Set().
func (di *DigitalInterrupt) Set(
	ctx context.Context, isHigh bool, extra map[string]interface{},
) error {
	return errors.New("cannot set value of a digital interrupt pin")
}

// Get reads the current value of the interrupt pin.
func (di *DigitalInterrupt) Get(ctx context.Context, extra map[string]interface{}) (bool, error) {
	value, err := di.line.Value()
	if err != nil {
		return false, err
	}

	// We'd expect value to be either 0 or 1, but any non-zero value should be considered high.
	return (value != 0), nil
}

// PWM is an unimplemented placeholder for GPIOPin.PWM().
func (di *DigitalInterrupt) PWM(ctx context.Context, extra map[string]interface{}) (float64, error) {
	return 0, errors.New("cannot get PWM of a digital interrupt pin")
}

// SetPWM is an unimplemented placeholder for GPIOPin.SetPWM().
func (di *DigitalInterrupt) SetPWM(
	ctx context.Context, dutyCyclePct float64, extra map[string]interface{},
) error {
	return errors.New("cannot set PWM of a digital interrupt pin")
}

// PWMFreq is an unimplemented placeholder for GPIOPin.PWMFreq().
func (di *DigitalInterrupt) PWMFreq(
	ctx context.Context, extra map[string]interface{},
) (uint, error) {
	return 0, errors.New("cannot get PWM freq of a digital interrupt pin")
}

// SetPWMFreq is an unimplemented placeholder for GPIOPin.SetPWMFreq().
func (di *DigitalInterrupt) SetPWMFreq(
	ctx context.Context, freqHz uint, extra map[string]interface{},
) error {
	return errors.New("cannot set PWM freq of a digital interrupt pin")
}
