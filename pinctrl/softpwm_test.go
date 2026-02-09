//go:build linux

package pinctrl

import (
	"context"
	"testing"
	"time"

	gl "go.viam.com/rdk/components/board/genericlinux"
	"go.viam.com/rdk/logging"
	"go.viam.com/test"
)

// createTestPinctrl creates a Pinctrl in testing mode with mock device tree.
func createTestPinctrl(t *testing.T) *Pinctrl {
	logger := logging.NewTestLogger(t)
	cfg := Config{
		GPIOChipPath: "gpio0",
		DevMemPath:   "/dev/gpiomem0",
		UseAlias:     true,
		ChipSize:     0x30000,
		TestPath:     "./mock-device-tree",
		UseGPIOMem:   true,
	}
	ctrl, err := SetupPinControl(cfg, logger)
	test.That(t, err, test.ShouldBeNil)
	return &ctrl
}

// createTestPin creates a GPIOPin for testing, soft PMW will work.
func createTestPin(ctrl *Pinctrl, pinNum int) *GPIOPin {
	mapping := gl.GPIOBoardMapping{
		GPIOChipDev: "/dev/gpiochip4",
		GPIO:        pinNum,
	}
	return ctrl.CreateGpioPin(mapping, 0)
}

func waitForCount(worker *softwarePWMWorker, expected int32) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if worker.Count() == expected {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestSoftwarePWMWorkerAddRemove(t *testing.T) {
	ctx := context.Background()
	ctrl := createTestPinctrl(t)
	defer ctrl.Close()

	worker := ctrl.pwmWorker
	test.That(t, worker.Count(), test.ShouldEqual, 0)

	pin1 := createTestPin(ctrl, 1)
	pin2 := createTestPin(ctrl, 2)
	pin3 := createTestPin(ctrl, 3)

	err := pin1.SetPWMFreq(ctx, 10, nil)
	test.That(t, err, test.ShouldBeNil)
	err = pin1.SetPWM(ctx, 0.5, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, waitForCount(worker, 1), test.ShouldBeTrue)

	err = pin2.SetPWMFreq(ctx, 20, nil)
	test.That(t, err, test.ShouldBeNil)
	err = pin2.SetPWM(ctx, 0.3, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, waitForCount(worker, 2), test.ShouldBeTrue)

	err = pin1.SetPWMFreq(ctx, 15, nil)
	test.That(t, err, test.ShouldBeNil)
	err = pin1.SetPWM(ctx, 0.7, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, waitForCount(worker, 2), test.ShouldBeTrue)

	// SetPWM(0) will return an error in test mode because it sets pin back to  LOW,
	err = pin1.SetPWM(ctx, 0, nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, waitForCount(worker, 1), test.ShouldBeTrue)

	err = pin3.SetPWMFreq(ctx, 5, nil)
	test.That(t, err, test.ShouldBeNil)
	err = pin3.SetPWM(ctx, 0.25, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, waitForCount(worker, 2), test.ShouldBeTrue)
}

func TestSoftPWMPinTimingCalculations(t *testing.T) {
	pwmPin := &softPWMPin{
		freqHz:       10.0,
		dutyCyclePct: 0.41,
		deadline:     time.Now(),
	}

	pwmPin.calculateTimes()

	expectedPeriod := 100 * time.Millisecond
	test.That(t, pwmPin.timeOn+pwmPin.timeOff, test.ShouldEqual, expectedPeriod)
	test.That(t, pwmPin.timeOn, test.ShouldEqual, 41*time.Millisecond)
	test.That(t, pwmPin.timeOff, test.ShouldEqual, (100-41)*time.Millisecond)

	pwmPin.dutyCyclePct = 0.25
	pwmPin.calculateTimes()

	test.That(t, pwmPin.timeOn, test.ShouldEqual, 25*time.Millisecond)
	test.That(t, pwmPin.timeOff, test.ShouldEqual, 75*time.Millisecond)

	pwmPin.dutyCyclePct = 0.5
	pwmPin.timeOn = 50 * time.Millisecond
	pwmPin.timeOff = 50 * time.Millisecond

	pwmPin.isOn = false
	before := time.Now()
	pwmPin.calculateNextDeadline()
	after := time.Now()

	test.That(t, pwmPin.isOn, test.ShouldBeTrue)
	test.That(t, pwmPin.deadline, test.ShouldHappenBetween, before.Add(pwmPin.timeOff), after.Add(pwmPin.timeOff))

	before = time.Now()
	pwmPin.calculateNextDeadline()
	after = time.Now()

	test.That(t, pwmPin.isOn, test.ShouldBeFalse)
	test.That(t, pwmPin.deadline, test.ShouldHappenBetween, before.Add(pwmPin.timeOn), after.Add(pwmPin.timeOn))
}

func TestSetCancelsSoftwarePWM(t *testing.T) {
	ctx := context.Background()
	ctrl := createTestPinctrl(t)
	defer ctrl.Close()

	pin := createTestPin(ctrl, 5)

	// Start software PWM
	err := pin.SetPWMFreq(ctx, 100, nil)
	test.That(t, err, test.ShouldBeNil)
	err = pin.SetPWM(ctx, 0.5, nil)
	test.That(t, err, test.ShouldBeNil)

	// Verify PWM is active
	test.That(t, waitForCount(ctrl.pwmWorker, 1), test.ShouldBeTrue)
	test.That(t, pin.usingSoftPWM, test.ShouldBeTrue)

	// Stop PWM on this pin
	err = pin.Set(ctx, true, nil)
	test.That(t, err, test.ShouldNotBeNil)

	test.That(t, waitForCount(ctrl.pwmWorker, 0), test.ShouldBeTrue)
	test.That(t, pin.usingSoftPWM, test.ShouldBeFalse)
}

func TestCloseRemovesPinFromPWM(t *testing.T) {
	ctx := context.Background()
	ctrl := createTestPinctrl(t)
	// Don't defer ctrl.Close() here since we're testing pin.Close()

	pin := createTestPin(ctrl, 9)

	err := pin.SetPWMFreq(ctx, 100, nil)
	test.That(t, err, test.ShouldBeNil)
	err = pin.SetPWM(ctx, 0.5, nil)
	test.That(t, err, test.ShouldBeNil)

	// Verify PWM is active
	test.That(t, waitForCount(ctrl.pwmWorker, 1), test.ShouldBeTrue)
	test.That(t, pin.usingSoftPWM, test.ShouldBeTrue)

	// Close the pin - should remove from PWM
	err = pin.Close()
	test.That(t, err, test.ShouldBeNil)

	test.That(t, waitForCount(ctrl.pwmWorker, 0), test.ShouldBeTrue)
	test.That(t, pin.usingSoftPWM, test.ShouldBeFalse)

	ctrl.Close()
}

func TestConcurrentSetAndPWMWorker(t *testing.T) {
	ctx := context.Background()
	ctrl := createTestPinctrl(t)
	defer ctrl.Close()

	pin := createTestPin(ctrl, 10)
	for range 100 {
		// Start software PWM
		err := pin.SetPWMFreq(ctx, 2000, nil) // High frequency to trigger many toggles
		test.That(t, err, test.ShouldBeNil)
		err = pin.SetPWM(ctx, 0.5, nil)
		test.That(t, err, test.ShouldBeNil)

		// Wait briefly for PWM to be active
		test.That(t, waitForCount(ctrl.pwmWorker, 1), test.ShouldBeTrue)

		// Immediately cancel with Set() - this races with the worker
		err = pin.Set(ctx, true, nil)
		test.That(t, err, test.ShouldNotBeNil)

		test.That(t, pin.usingSoftPWM, test.ShouldBeFalse)
	}
}
