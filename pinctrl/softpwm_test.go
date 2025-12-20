//go:build linux

package pinctrl

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/test"
)

const tolerance float64 = 0.04

// pinEvent records a PWM toggle event.
type pinEvent struct {
	pinNumber int
	state     bool
	timestamp time.Time
}

// eventCollector collects PWM events.
type eventCollector struct {
	mu     sync.Mutex
	events map[int][]pinEvent
}

func newEventCollector() *eventCollector {
	return &eventCollector{
		events: make(map[int][]pinEvent),
	}
}

func (c *eventCollector) addEvent(pinNumber int, state bool, timestamp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.events[pinNumber] == nil {
		c.events[pinNumber] = make([]pinEvent, 0)
	}
	c.events[pinNumber] = append(c.events[pinNumber], pinEvent{
		pinNumber: pinNumber,
		state:     state,
		timestamp: timestamp,
	})
}

func (c *eventCollector) getEvents(pinNumber int) []pinEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]pinEvent, len(c.events[pinNumber]))
	copy(result, c.events[pinNumber])
	return result
}

// calcFreqAndDutyCycle calculates the estimated frequency and duty cycle from recorded events.
func (c *eventCollector) calcFreqAndDutyCycle(pinNumber int) (freqHz, dutyCycle float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	events := c.events[pinNumber]
	if len(events) < 4 {
		return 0, 0
	}

	// Calculate average period and duty cycle from transitions
	var totalOnTime time.Duration
	var totalOffTime time.Duration
	var cycles int

	for i := range len(events) - 1 {
		duration := events[i+1].timestamp.Sub(events[i].timestamp)
		if events[i].state {
			totalOnTime += duration
		} else {
			totalOffTime += duration
			cycles++
		}
	}

	if cycles == 0 {
		return 0, 0
	}

	avgPeriod := (totalOnTime + totalOffTime) / time.Duration(cycles)
	if avgPeriod == 0 {
		return 0, 0
	}

	freqHz = float64(time.Second) / float64(avgPeriod)
	dutyCycle = float64(totalOnTime) / float64(totalOnTime+totalOffTime)

	return freqHz, dutyCycle
}

type fakeGPIOPin struct {
	pinNumber    int
	collector    *eventCollector
	state        bool
	freqHz       uint
	dutyCyclePct float64
	pwmWorker    *softwarePWMWorker
}

func newFakeGPIOPin(pinNumber int, collector *eventCollector, worker *softwarePWMWorker) *fakeGPIOPin {
	return &fakeGPIOPin{
		pinNumber: pinNumber,
		collector: collector,
		pwmWorker: worker,
	}
}

func (p *fakeGPIOPin) Set(ctx context.Context, high bool, extra map[string]interface{}) error {
	p.state = high
	p.collector.addEvent(p.pinNumber, high, time.Now())
	return nil
}

func (p *fakeGPIOPin) Get(ctx context.Context, extra map[string]interface{}) (bool, error) {
	return p.state, nil
}

func (p *fakeGPIOPin) PWM(ctx context.Context, extra map[string]interface{}) (float64, error) {
	return p.dutyCyclePct, nil
}

func (p *fakeGPIOPin) SetPWM(ctx context.Context, dutyCyclePct float64, extra map[string]interface{}) error {
	p.dutyCyclePct = dutyCyclePct
	return p.setupPWM()
}

func (p *fakeGPIOPin) PWMFreq(ctx context.Context, extra map[string]interface{}) (uint, error) {
	return p.freqHz, nil
}

func (p *fakeGPIOPin) SetPWMFreq(ctx context.Context, freqHz uint, extra map[string]interface{}) error {
	p.freqHz = freqHz
	return p.setupPWM()
}

func (p *fakeGPIOPin) setupPWM() error {
	if p.pwmWorker == nil {
		return nil
	}
	if p.freqHz == 0 || p.dutyCyclePct == 0 {
		return p.pwmWorker.RemovePin(p)
	}
	return p.pwmWorker.AddPin(p, float64(p.freqHz), p.dutyCyclePct)
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
	worker := newSoftwarePWMWorker(logging.NewTestLogger(t))
	defer worker.Stop()

	collector := newEventCollector()

	test.That(t, worker.Count(), test.ShouldEqual, 0)

	pin1 := newFakeGPIOPin(1, collector, worker)
	pin2 := newFakeGPIOPin(2, collector, worker)
	pin3 := newFakeGPIOPin(3, collector, worker)

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

	err = pin1.SetPWM(ctx, 0, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, waitForCount(worker, 1), test.ShouldBeTrue)

	err = pin3.SetPWMFreq(ctx, 5, nil)
	test.That(t, err, test.ShouldBeNil)
	err = pin3.SetPWM(ctx, 0.25, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, waitForCount(worker, 2), test.ShouldBeTrue)
}

func TestSoftwarePWMSinglePin(t *testing.T) {
	ctx := context.Background()
	testFreqHz := uint(2)
	testDutyCycle := 0.33
	testDuration := 5 * time.Second

	collector := newEventCollector()
	pinNumber := 1

	worker := newSoftwarePWMWorker(logging.NewTestLogger(t))
	fakePin := newFakeGPIOPin(pinNumber, collector, worker)

	err := fakePin.SetPWMFreq(ctx, testFreqHz, nil)
	test.That(t, err, test.ShouldBeNil)
	err = fakePin.SetPWM(ctx, testDutyCycle, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, waitForCount(worker, 1), test.ShouldBeTrue)

	time.Sleep(testDuration)
	worker.Stop()
	events := collector.getEvents(pinNumber)
	// t.Logf("Collected %d events over %v", len(events), testDuration)

	expectedToggles := int(testDuration.Seconds() * float64(testFreqHz) * 2)
	test.That(t, len(events), test.ShouldBeGreaterThanOrEqualTo, expectedToggles-1)
	test.That(t, len(events), test.ShouldBeLessThanOrEqualTo, expectedToggles+1)

	measuredFreq, measuredDuty := collector.calcFreqAndDutyCycle(pinNumber)

	// t.Logf("Expected: freq=%d Hz, duty=%.2f%%", fakePin.freqHz, fakePin.dutyCyclePct*100)
	// t.Logf("Measured: freq=%.2f Hz, duty=%.2f%%", measuredFreq, measuredDuty*100)

	test.That(t, measuredFreq, test.ShouldBeBetween, float64(fakePin.freqHz)*(1-tolerance), float64(fakePin.freqHz)*(1+tolerance))
	test.That(t, measuredDuty, test.ShouldBeBetween, fakePin.dutyCyclePct*(1-tolerance), fakePin.dutyCyclePct*(1+tolerance))
}

func TestSoftwarePWMSlowFrequency(t *testing.T) {
	ctx := context.Background()
	testFreqHz := uint(1)
	testDutyCycle := 0.33
	testDuration := 5 * time.Second

	collector := newEventCollector()
	pinNumber := 1

	worker := newSoftwarePWMWorker(logging.NewTestLogger(t))
	fakePin := newFakeGPIOPin(pinNumber, collector, worker)

	err := fakePin.SetPWMFreq(ctx, testFreqHz, nil)
	test.That(t, err, test.ShouldBeNil)
	err = fakePin.SetPWM(ctx, testDutyCycle, nil)
	test.That(t, err, test.ShouldBeNil)

	time.Sleep(testDuration)
	worker.Stop()

	events := collector.getEvents(pinNumber)
	//	t.Logf("Collected %d events over %v at %dHz", len(events), testDuration, testFreqHz)

	test.That(t, len(events), test.ShouldBeGreaterThanOrEqualTo, 2)

	if len(events) >= 2 {
		test.That(t, events[0].state, test.ShouldBeTrue)
		test.That(t, events[1].state, test.ShouldBeFalse)

		expectedOnDuration := time.Duration(float64(time.Second) / float64(testFreqHz) * testDutyCycle)
		actualOnDuration := events[1].timestamp.Sub(events[0].timestamp)

		// t.Logf("Expected ON duration: %v, Actual: %v", expectedOnDuration, actualOnDuration)

		test.That(t, actualOnDuration, test.ShouldBeBetween,
			time.Duration(float64(expectedOnDuration)*(1-tolerance)),
			time.Duration(float64(expectedOnDuration)*(1+tolerance)))
	}
}

func TestSoftwarePWMMultiplePins(t *testing.T) {
	ctx := context.Background()
	const (
		numPins       = 30
		testFreqHz    = uint(500)
		testDutyCycle = 0.25
		testDuration  = 5 * time.Second
	)

	collector := newEventCollector()
	worker := newSoftwarePWMWorker(logging.NewTestLogger(t))

	fakePins := make([]*fakeGPIOPin, numPins)
	for i := range numPins {
		pinNumber := i + 1
		freqHz := testFreqHz + uint(10*i)
		fakePins[i] = newFakeGPIOPin(pinNumber, collector, worker)
		err := fakePins[i].SetPWMFreq(ctx, freqHz, nil)
		test.That(t, err, test.ShouldBeNil)
		err = fakePins[i].SetPWM(ctx, testDutyCycle, nil)
		test.That(t, err, test.ShouldBeNil)
	}

	test.That(t, waitForCount(worker, numPins), test.ShouldBeTrue)

	time.Sleep(testDuration)

	worker.Stop()

	for i := range numPins {
		pin := fakePins[i]
		events := collector.getEvents(pin.pinNumber)

		// t.Logf("Pin %d: Collected %d events over %v", pin.pinNumber, len(events), testDuration)

		expectedToggles := int(testDuration.Seconds() * float64(pin.freqHz) * 2)
		test.That(t, len(events), test.ShouldBeGreaterThanOrEqualTo, expectedToggles-50) // Allow some margin
		test.That(t, len(events), test.ShouldBeLessThanOrEqualTo, expectedToggles+50)

		measuredFreq, measuredDuty := collector.calcFreqAndDutyCycle(pin.pinNumber)

		// t.Logf("Pin %d: Expected freq=%d Hz, duty=%.2f%%; Measured freq=%.2f Hz, duty=%.2f%%",
		//	pin.pinNumber, pin.freqHz, pin.dutyCyclePct*100, measuredFreq, measuredDuty*100)

		test.That(t, measuredFreq, test.ShouldBeBetween, float64(pin.freqHz)*(1-tolerance), float64(pin.freqHz)*(1+tolerance))
		test.That(t, measuredDuty, test.ShouldBeBetween, pin.dutyCyclePct*(1-tolerance), pin.dutyCyclePct*(1+tolerance))
	}
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
