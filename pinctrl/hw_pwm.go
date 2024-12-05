//go:build linux

// Package pinctrl implements pinctrl
package pinctrl

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	mmap "github.com/edsrzf/mmap-go"
	"go.viam.com/rdk/logging"
	goutils "go.viam.com/utils"
)

// There are times when we need to set the period to some value, any value. It must be a positive
// number of nanoseconds, but some boards (e.g., the Jetson Orin) cannot tolerate periods below 1
// microsecond. We'll use 1 millisecond, for added confidence that all boards should support it.
const safePeriodNs = 1e6

/*
GPIO Pin / Bank Information for 'Alternative Modes'.
** Note: In Raspberry Pi documentation & code, alternative Mode information is generally denoted using the keyword FSEL (function select) **

Each group of pins belongs to a bank, which has its own portion of memory in the gpio chip.
Each bank has its own base address, which is a fixed offset from the base address
of the virtual page pointing to the gpio chip data in memory:

	bank0 = 0x0000
	bank1 = 0x4000
	bank2 = 0x8000

'bankDivisions' stores the GPIO# of the first pin in each bank. Here, there are 3 banks:

	Bank0: GPIO pins 1-27
	Bank1: GPIO pins 28-33
	Bank2: GPIO pins 34-54

numGPIOPins provides an upper bound for the pin number when calculating what bank a GPIO pin belongs to.

Typical use of the Pi5 only involves bank 0, which supports GPIO Pins 1-27, so the other offsets can be commented out.
If we ever wanted to support more than the 27 GPIO Pins on the standard pi5 board, these would be relevant:

	const bank0Offset = 0x0000
	const bank1Offset = 0x4000
	const bank2Offset = 0x8000
	var bankDivisions = []int{1, 28, 34, numGPIOPins + 1}
	var bankOffsets = []int{bank0Offset, bank1Offset, bank2Offset}

Since all of our pins are stored in bank0, we only retrieve pin data from bank0.
Bank0 starts at offset 0x0000, so we don't need to add an offset either.
*/

// On a pi5 without peripherals, there are 27 GPIO Pins. This is the max number of GPIO Pins supported by the pi5 w peripherals is 54.
const numGPIOPins = 27

/*
These modes are used during pin control. Depending on which mode we'd like a pwm pin to
be in (either GPIO or PWM), we overwrite a pin's mode data with one of these values.

Note: Based on my (Maria's) experimentation, I'm pretty sure that ALT5 is the 'default' mode that tells the processor not
to use alternative modes at all. In the pi5 library, they denote that SYS_RIO = ALT5, and my assumption here is
that RIO = regular input output. regardless of whether I set a pin's mode to IP or OP, the byte is still set to ALT5.
GPIO usage uses different banks, addresses, registers, etc, so I'm pretty sure this is a way of telling the processor it
shouldn't be using the alternative mode data at all.
*/
const (
	ALT3 byte = 0x03 // PWM MODE
	ALT5 byte = 0x05 // GPIO MODE

	PWMMode  byte = ALT3
	GPIOMode byte = ALT5
)

type pwmDevice struct {
	chipPath string
	pwmID    int
	line     int

	// We have no mutable state, but the mutex is used to write to multiple pseudofiles atomically.
	mu     sync.Mutex
	logger logging.Logger

	// virtual page that maps to memory associated with gpiochip0. For a given gpio pin on the gpio chip (which stores all the pins),
	//  we will overwrite bytes here to switch between GPIO and PWM mode.
	gpioPinsPage *mmap.MMap
}

// TODO: we currently use sysfs to set the hardware pwm, and pinctrl(/dev/mem) for setting the pin mode.
// Eventually we want to set the hardware pwm using pinctrl instead.
func newPwmDevice(chipPath string, pwmID, line int, logger logging.Logger, gpioPinsPage *mmap.MMap) *pwmDevice {
	return &pwmDevice{chipPath: chipPath, pwmID: pwmID, line: line, logger: logger, gpioPinsPage: gpioPinsPage}
}

func writeValue(filepath string, value uint64, logger logging.Logger) error {
	logger.Debugf("Writing %d to %s", value, filepath)
	//nolint:perfsprint
	data := []byte(fmt.Sprintf("%d", value))
	// The file permissions (the third argument) aren't important: if the file needs to be created,
	// something has gone horribly wrong!
	err := os.WriteFile(filepath, data, 0o600)
	// Some errors (e.g., trying to unexport an already-unexported pin) should get suppressed. If
	// we're trying to debug something in here, log the error even if it will later be ignored.
	if err != nil {
		logger.Debugf("Encountered error writing to sysfs: %s", err)
		return errors.Join(err, errors.New(filepath))
	}
	return nil
}

func (pwm *pwmDevice) writeChip(filename string, value uint64) error {
	return writeValue(fmt.Sprintf("%s/%s", pwm.chipPath, filename), value, pwm.logger)
}

func (pwm *pwmDevice) linePath() string {
	return fmt.Sprintf("%s/pwm%d", pwm.chipPath, pwm.pwmID)
}

func (pwm *pwmDevice) writeLine(filename string, value uint64) error {
	return writeValue(fmt.Sprintf("%s/%s", pwm.linePath(), filename), value, pwm.logger)
}

// Export tells the OS that this pin is in use, and enables configuration via sysfs.
func (pwm *pwmDevice) export() error {
	if _, err := os.Lstat(pwm.linePath()); err != nil {
		if os.IsNotExist(err) {
			// happy path
			return pwm.writeChip("export", uint64(pwm.pwmID))
		}
		return err // Something unexpected has gone wrong.
	}
	// Otherwise, the line we're trying to export already exists.
	pwm.logger.Debugf("Skipping re-export of already-exported line %d on HW PWM chip %s",
		pwm.pwmID, pwm.chipPath)
	return nil
}

// Unexport turns off any PWM signal the pin was providing, and tells the OS that this pin is no
// longer in use (so it can be reused as an input pin, etc.).
func (pwm *pwmDevice) unexport() error {
	if _, err := os.Lstat(pwm.linePath()); err != nil {
		if os.IsNotExist(err) {
			pwm.logger.Debugf("Skipping unexport of already-unexported line %d on HW PWM chip %s",
				pwm.pwmID, pwm.chipPath)
			return nil
		}
		return err // Something has gone wrong.
	}

	// If we unexport the pin while it is enabled, it might continue outputting a PWM signal,
	// causing trouble if you start using the pin for something else. So, we need to disable it.
	// However, on certain boards (e.g., the Beaglebone AI64), disabling an already-disabled PWM
	// device results in an error. We don't care if there's an error: it should be disabled no
	// matter what.
	goutils.UncheckedError(pwm.disable())

	// On boards like the Odroid C4, there is a race condition in the kernel where, if you unexport
	// the pin too quickly after changing something else about it (e.g., disabling it), the whole
	// PWM system gets corrupted. Sleep for a small amount of time to avoid this.
	time.Sleep(10 * time.Millisecond)
	if err := pwm.writeChip("unexport", uint64(pwm.pwmID)); err != nil {
		return err
	}

	// This should be a redundant call because switching to GPIO happens implicitly.
	if err := pwm.SetPinMode(GPIOMode); err != nil {
		return err
	}

	return nil
}

// Enable tells an exported pin to output the PWM signal it has been configured with.
func (pwm *pwmDevice) enable() error {
	// There is no harm in enabling an already-enabled pin; no errors will be returned if we try.
	return pwm.writeLine("enable", 1)
}

// Disable tells an exported pin to stop outputting its PWM signal, but it is still available for
// reconfiguring and re-enabling.
func (pwm *pwmDevice) disable() error {
	// There is no harm 	in disabling an already-disabled pin; no errors will be returned if we try.
	return pwm.writeLine("enable", 0)
}

// Only call this from public functions, to avoid double-wrapping the errors.
func (pwm *pwmDevice) wrapError(err error) error {
	if err != nil {
		return errors.Join(err, fmt.Errorf("HW PWM chipPath %s, line %d", pwm.chipPath, pwm.pwmID))
	}
	return nil
}

/*
For all pins belonging to the same bank, pin data is stored contiguously and in 8 byte chunks.
For a given pin, this method determines:
 1. which bank the pin belongs to
    Currently this code is specific to the pi5, so we know that all of our pins have to be within bank 0.
    Eventually we should change this to handle other banks
 2. the starting address of its 8 byte data chunk.

note: numGPIOPins is a hardcoded value for the pi5
*/
func getGPIOPinAddress(gpioNumber int) (int64, error) {
	const pinDataSizeBytes = 0x8 // 8 bytes per pin: 4 bytes represent control statuses and 4 bytes are for different control modes

	// check that the given pin is in bank 0(it should be)
	if !(1 <= gpioNumber && gpioNumber <= numGPIOPins) {
		return -1, errors.New("pin is out of bank range")
	}

	pinAddressOffset := (gpioNumber * pinDataSizeBytes)
	return int64(pinAddressOffset), nil
}

// updates the given mode of a pin by finding its specific location in memory & writing to the 'mode' byte in the 8 byte block of pin data.
func (pwm *pwmDevice) SetPinMode(pinMode byte) error {
	/*
		Remember that GPIO mode data is stored in a different area of memory, and this area of memory relates to
		setting alternative modes; in this case we set ALT3 (PWM Mode). When we want to use GPIO Mode, we will set the
		alternative mode to ALT5 (default mode), which tells the processor to go look in the GPIO data area (SYS_RIO) for instructions.

		Of the 8 bytes representing all of a given pin's alternative mode data:
			pinBytes[0:3] -> bytes are allocated for 'status' modes (still unsure of what status does).
			pinBytes[4:7] -> bytes are allocated for alternative modes.

		However, you only need 1 byte to represent all 8 modes, so only the first mode byte (pinBytes[4]) is written to when changing modes.
		The remaning 3 mode bytes are just 0xff's.
	*/

	// Find base address of GPIO Pin in memory:
	pinAddress, err := getGPIOPinAddress(pwm.line)
	if err != nil {
		return fmt.Errorf("error getting gpio pin address: %w", err)
	}

	// Get pin's memory contents from virtual page; set the 5th byte of pin to mode
	altModeIndex := int64(4)
	vPage := *(pwm.gpioPinsPage)
	vPage[pinAddress+altModeIndex] = pinMode

	return nil
}

// SetPwm configures an exported pin and enables its output signal.
// Warning: if this function returns a non-nil error, it could leave the pin in an indeterminate
// state. Maybe it's exported, maybe not. Maybe it's enabled, maybe not. The new frequency and duty
// cycle each might or might not be set.
func (pwm *pwmDevice) SetPwm(freqHz uint, dutyCycle float64) (err error) {
	pwm.mu.Lock()
	defer pwm.mu.Unlock()

	// If there is ever an error in here, annotate it with which sysfs device and line we're using.
	defer func() {
		err = pwm.wrapError(err)
	}()
	// Set pin mode to hardware pwm enabled before using PWM.
	if err := pwm.SetPinMode(PWMMode); err != nil {
		return err
	}
	// Every time this pin is used as a (non-PWM) GPIO input or output, it gets unexported on the
	// PWM chip. Make sure to re-export it here.
	if err := pwm.export(); err != nil {
		return err
	}

	// Intuitively, we should disable the pin, set the new parameters, and then enable it again.
	// However, the BeagleBone AI64 has a weird quirk where you need to enable the pin *before* you
	// set the parameters, because enabling it afterwards sets the pin constantly high until the
	// period or duty cycle is modified again. So, enable the PWM signal first and *then* set it to
	// the correct values. This shouldn't hurt anything on the other boards; it's just not the
	// intuitive order.
	if err := pwm.enable(); err != nil {
		// If the board is newly booted up, the period (and everything else) might be initialized
		// to 0, and enabling the pin with a period of 0 results in errors. Let's try making the
		// period non-zero and enabling it again.
		pwm.logger.Debugf("Cannot enable HW PWM device %s line %d, will try changing period: %s",
			pwm.chipPath, pwm.pwmID, err)
		if err := pwm.writeLine("period", safePeriodNs); err != nil {
			return err
		}
		// Now, try enabling the pin one more time before giving up.
		if err := pwm.enable(); err != nil {
			return err
		}
	}

	// Sysfs has a pseudofile named duty_cycle which contains the number of nanoseconds that the
	// pin should be high within a period. It's not how the rest of the world defines a duty cycle,
	// so we will refer to it here as the active duration.
	periodNs := 1e9 / uint64(freqHz)
	activeDurationNs := uint64(float64(periodNs) * dutyCycle)

	// If we ever try setting the active duration higher than the period (or the period lower than
	// the active duration), we will get an error. So, make sure we never do that!

	// The BeagleBone has a weird quirk where, if you don't change the period or active duration
	// after enabling the PWM line, it just goes high and stays there, rather than blinking at the
	// intended rate. To avoid this, we first set the active duration to 0 and the period to 1
	// microsecond, and then set the period and active duration to their intended values. That way,
	// if you turn the PWM signal off and on again, it still works because you've changed the
	// values after (re-)enabling the line.

	// Setting the active duration to 0 should always work: this is guaranteed to be less than the
	// period, unless the period in zero. In that case, just ignore the error.
	goutils.UncheckedError(pwm.writeLine("duty_cycle", 0))

	// Now that the active duration is 0, setting the period to any number should work.
	if err := pwm.writeLine("period", safePeriodNs); err != nil {
		return err
	}

	// Same thing here: the active duration is 0, so any value should work for the period.
	if err := pwm.writeLine("period", periodNs); err != nil {
		return err
	}

	// Now that the period is set to its intended value, there should be no trouble setting the
	// active duration, which is guaranteed to be at most the period.
	if err := pwm.writeLine("duty_cycle", activeDurationNs); err != nil {
		return err
	}

	return nil
}

func (pwm *pwmDevice) Close() error {
	pwm.mu.Lock()
	defer pwm.mu.Unlock()
	return pwm.wrapError(pwm.unexport())
}
