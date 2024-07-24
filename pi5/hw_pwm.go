//go:build linux

// Package genericlinux is for Linux boards. This particular file is for using sysfs to
// interact with PWM devices. All of these functions are idempotent: you can double-export a pin or
// double-close it with no problems.
package pi5

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	mmap "github.com/edsrzf/mmap-go"
	"github.com/pkg/errors"
	goutils "go.viam.com/utils"

	"go.viam.com/rdk/logging"
)

// There are times when we need to set the period to some value, any value. It must be a positive
// number of nanoseconds, but some boards (e.g., the Jetson Orin) cannot tolerate periods below 1
// microsecond. We'll use 1 millisecond, for added confidence that all boards should support it.
const safePeriodNs = 1e6

type pwmDevice struct {
	chipPath string
	line     int

	// We have no mutable state, but the mutex is used to write to multiple pseudofiles atomically.
	mu     sync.Mutex
	logger logging.Logger

	// this is the virtual page that maps to memory associated with gpiochip0. We will overwrite bytes
	// here to change a pin's mode from GPIO mode to HWPWM mode.
	gpioChipVPagePtr *mmap.MMap
}

func newPwmDevice(chipPath string, line int, logger logging.Logger, gpioChipVPagePtr *mmap.MMap) *pwmDevice {
	return &pwmDevice{chipPath: chipPath, line: line, logger: logger, gpioChipVPagePtr: gpioChipVPagePtr}
}

func writeValue(filepath string, value uint64, logger logging.Logger) error {
	logger.Debugf("Writing %d to %s", value, filepath)
	data := []byte(fmt.Sprintf("%d", value))
	// The file permissions (the third argument) aren't important: if the file needs to be created,
	// something has gone horribly wrong!
	err := os.WriteFile(filepath, data, 0o600)
	// Some errors (e.g., trying to unexport an already-unexported pin) should get suppressed. If
	// we're trying to debug something in here, log the error even if it will later be ignored.
	if err != nil {
		logger.Debugf("Encountered error writing to sysfs: %s", err)
	}
	return errors.Wrap(err, filepath)
}

func (pwm *pwmDevice) writeChip(filename string, value uint64) error {
	str := fmt.Sprintf("%s/%s", pwm.chipPath, filename)
	fmt.Printf(str)
	return writeValue(fmt.Sprintf("%s/%s", pwm.chipPath, filename), value, pwm.logger)
}

func (pwm *pwmDevice) linePath() string {
	return fmt.Sprintf("%s/pwm%d", pwm.chipPath, pwm.line)
}

func (pwm *pwmDevice) writeLine(filename string, value uint64) error {
	return writeValue(fmt.Sprintf("%s/%s", pwm.linePath(), filename), value, pwm.logger)
}

// Export tells the OS that this pin is in use, and enables configuration via sysfs.
func (pwm *pwmDevice) export() error {
	if _, err := os.Lstat(pwm.linePath()); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("// The pseudofile we're trying to export doesn't yet exist. Export it now. This is the")
			// happy path
			return pwm.writeChip("export", uint64(pwm.line))
		}
		return err // Something unexpected has gone wrong.
	}
	// Otherwise, the line we're trying to export already exists.
	pwm.logger.Debugf("Skipping re-export of already-exported line %d on HW PWM chip %s",
		pwm.line, pwm.chipPath)
	return nil
}

// Unexport turns off any PWM signal the pin was providing, and tells the OS that this pin is no
// longer in use (so it can be reused as an input pin, etc.).
func (pwm *pwmDevice) unexport() error {
	if _, err := os.Lstat(pwm.linePath()); err != nil {
		if os.IsNotExist(err) {
			pwm.logger.Debugf("Skipping unexport of already-unexported line %d on HW PWM chip %s",
				pwm.line, pwm.chipPath)
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
	if err := pwm.writeChip("unexport", uint64(pwm.line)); err != nil {
		return err
	}

	// This should a be redundant call because switching to GPIO happens implicitly.
	// function uses pwm.line to determine what GPIO Pin it needs to set to the inputted mode.
	if err := pwm.SetPinMode(GPIO); err != nil {
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
	// Note that if err is nil, errors.Wrap() will return nil, too.
	return errors.Wrapf(err, "HW PWM chipPath %s, line %d", pwm.chipPath, pwm.line)
}

func (pwm *pwmDevice) callGPIOReadall() {
	command := "gpio"
	args := []string{"readall"}

	_, err := exec.LookPath(command)
	if err != nil {
		fmt.Printf("Command %s not found in PATH\n", command)
		return
	}

	cmd := exec.Command(command, args...)
	output, err := cmd.Output()
	if err != nil {
		fmt.Println("Error executing command:", err)
		return
	}

	// Convert output to string and split into lines
	outputStr := string(output)
	lines := strings.Split(outputStr, "\n")

	// Define the physical pins to filter
	targetPins := []string{" 35 ||"}

	// Iterate over lines and print those that match the target pins
	for _, line := range lines {
		for _, pin := range targetPins {
			if strings.Contains(line, pin) {
				parts := strings.Split(line, "||")
				if len(parts) > 1 {
					fmt.Println(parts[0])
				}
			}
		}
	}
}

/*
For all Pins belonging to the same bank, pin data is stored contiguously and in 8 byte chunks.
For a given pin, this method determines:
 1. which bank the pin belongs to
 2. the starting address of its 8 byte data chunk
*/
func getGPIOPinAddress(GPIONumber int) (int64, error) {

	// Regarding the header: I (Maria) am unsure about what is stored here. It might just be GPIO 0.
	// In that case, banks1 & 2 would be wrong for including the header in offset calcs.
	bankHeaderSizeBytes := 8 // 8 bytes of either header data assosciated with a bank

	if !(1 <= GPIONumber && GPIONumber <= maxGPIOPins) {
		return -1, errors.New("pin is out of bank range")
	}

	for i := 0; i < len(bankDivisions)-1; i++ {
		if bankDivisions[i] <= GPIONumber && GPIONumber < bankDivisions[i+1] {

			bankNum := i
			bankBaseAddr := fselBankOffsets[bankNum]
			pinBankOffset := GPIONumber - bankDivisions[i]

			pinAddressOffset := bankBaseAddr + bankHeaderSizeBytes + ((pinBankOffset) * fselPinDataSize)
			return int64(pinAddressOffset), nil
		}
	}

	return -1, errors.New("pin in bank range but not set")
}

// This method updates the given mode of a pin by finding its specific location in memory & writing to the 'mode' byte in the 8 byte block of pin data.
func (pwm *pwmDevice) writeToPinModeByte(GPIONumber int, newMode byte) error {

	// Of the 8 bytes that represent a given pin's data, only the 4th index corresponds to the alternative mode setting
	altModeIndex := 4

	pinAddress, err := getGPIOPinAddress(GPIONumber)
	if err != nil {
		return fmt.Errorf("error getting gpio bank number: %w", err)
	}

	// find pin data within virtual page; retrieve the 4th byte from the pin data
	vPage := *(pwm.gpioChipVPagePtr)
	pinBytes := vPage[pinAddress : pinAddress+fselPinDataSize]
	altModeByte := pinBytes[altModeIndex]

	// We keep the left half of the byte preserved, only modifying the right half
	// Preserve Left Side of Byte using this Mask
	leftSideMask := byte(0xf0)

	// Set Right Side of Byte ONLY using this Mask. Ensures left side cannot be overwritten
	rightSideMask := byte(0x0f)

	// Set new mode via write to correct while protecting previous other settings
	newAltModeByte := (altModeByte & leftSideMask) | (newMode & rightSideMask)
	pinBytes[4] = newAltModeByte

	return nil
}

/*
Each pwm line corresponds to a GPIO Pin. The pi5 mapping is:

	PWM Line 0 -> GPIO 12
	PWM Line 1 -> GPIO 13
	PWM Line 2 -> GPIO 18
	PWM Line 3 -> GPIO 19

Other mappings for different pis can be found here:  https://pypi.org/project/rpi-hardware-pwm/#modal-close

Use the writeToPinModeByte to set the mode to PWM or GPIO using this helper method. Pin Mode will either be 'HPWM' or 'GPIO'
*/
func (pwm *pwmDevice) SetPinMode(pinMode byte) (err error) {
	switch pwm.line {
	case 0:
		err = pwm.writeToPinModeByte(12, pinMode)
	case 1:
		err = pwm.writeToPinModeByte(13, pinMode)
	case 2:
		err = pwm.writeToPinModeByte(18, pinMode)
	case 3:
		err = pwm.writeToPinModeByte(19, pinMode)
	}

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

	// Set pin mode to hardware pwm enabled before using for PWM.
	// function uses pwm.line to determine what GPIO Pin it needs to set to the inputted mode.
	if err := pwm.SetPinMode(HPWM); err != nil {
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
			pwm.chipPath, pwm.line, err)
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
