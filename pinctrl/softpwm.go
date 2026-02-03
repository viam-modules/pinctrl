//go:build linux

package pinctrl

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/utils"
)

type pwmOpType int

const (
	opAddPin pwmOpType = iota
	opRemovePin
)

// maxPendingOps is the number of pin operation (change in configuration) that can pend. In theory 32 can never be exceeded.
const maxPendingOps = 32

// pwmOp represents an operation to be processed by the worker.
type pwmOp struct {
	opType       pwmOpType
	pin          *GPIOPin
	freqHz       float64
	dutyCyclePct float64
}

// softPWMPin implement a single pin managed by the software PWM worker.
type softPWMPin struct {
	deadline     time.Time
	dutyCyclePct float64
	freqHz       float64
	pin          *GPIOPin
	timeOn       time.Duration
	timeOff      time.Duration
	isOn         bool
}

// calculateNextDeadline updates the deadline based on the current state.
func (p *softPWMPin) calculateNextDeadline() {
	if p.isOn {
		// Currently on, next deadline is when we turn off
		p.deadline = time.Now().Add(p.timeOn)
		p.isOn = false
	} else {
		// Currently off, next deadline is when we turn on
		p.deadline = time.Now().Add(p.timeOff)
		p.isOn = true
	}
}

func (p *softPWMPin) calculateTimes() {
	oldTimeOn := p.timeOn
	oldTimeOff := p.timeOff

	if p.freqHz <= 0 {
		p.timeOn = 0
		p.timeOff = 0
		return
	}

	period := time.Duration(float64(time.Second) / p.freqHz)
	p.timeOn = time.Duration(float64(period) * p.dutyCyclePct)
	p.timeOff = period - p.timeOn

	// Adjust deadline based on current state
	if p.isOn {
		p.deadline = p.deadline.Add(p.timeOn - oldTimeOn)
	} else {
		p.deadline = p.deadline.Add(p.timeOff - oldTimeOff)
	}
}

func (p *softPWMPin) toggle() error {
	newState := !p.isOn

	p.pin.mu.Lock()

	if !p.pin.usingSoftPWM {
		p.pin.mu.Unlock()
		return nil
	}
	err := p.pin.setInternal(newState)
	p.pin.mu.Unlock()
	now := time.Now()

	// regardless of the error returned update the deadline and capture the new state
	p.isOn = newState
	if newState {
		p.deadline = now.Add(p.timeOn)
	} else {
		p.deadline = now.Add(p.timeOff)
	}

	return err
}

// softwarePWMWorker manages all software PWM pins with a single goroutine.
// Fields are read and written from the worker so there is no need for a mutex.
type softwarePWMWorker struct {
	opChan chan *pwmOp
	count  atomic.Int32
	worker *utils.StoppableWorkers
	pins   *list.List // list of *softPWMPin, ordered by earliest deadline first
	logger logging.Logger
}

func newSoftwarePWMWorker(log logging.Logger) *softwarePWMWorker {
	w := &softwarePWMWorker{
		opChan: make(chan *pwmOp, maxPendingOps),
		pins:   list.New(),
		logger: log,
	}
	w.worker = utils.NewBackgroundStoppableWorkers(w.loop)
	return w
}

func (w *softwarePWMWorker) loop(ctx context.Context) {
	for {
		func() {
			// when no pin are configured let's wait till one is
			if w.pins.Len() == 0 {
				select {
				case <-ctx.Done():
					return
				case op := <-w.opChan:
					w.processOp(op)
				}
			}
			// drain any pending operation
			for {
				select {
				case op := <-w.opChan:
					w.processOp(op)
				default:
					return
				}
			}
		}()

		// Process pins and get sleep duration
		sleepDuration, err := func() (time.Duration, error) {
			for {
				// if our context has been canceled return early
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				headElem := w.pins.Front()
				if headElem == nil {
					return 0, nil
				}
				headPin := headElem.Value.(*softPWMPin)
				now := time.Now()
				// if the deadline is in the future let's sleep a bit
				if now.Before(headPin.deadline) {
					return headPin.deadline.Sub(now), nil
				}
				if err := headPin.toggle(); err != nil {
					// this may be very spammy maybe we should ignore?
					w.logger.Errorf("error %v when changing the state of a pin", err)
				}
				w.repositionElement(headElem, headPin.deadline)
			}
		}()
		if err != nil {
			return
		}
		if sleepDuration == 0 {
			continue
		}

		if !accurateSleep(ctx, sleepDuration) {
			return
		}
	}
}

func (w *softwarePWMWorker) processOp(op *pwmOp) {
	switch op.opType {
	case opAddPin:
		w.addPinInternal(op.pin, op.freqHz, op.dutyCyclePct)

	case opRemovePin:
		w.removePinInternal(op.pin)
	}
}

// repositionElement moves the element to its correct position based on the new deadline.
func (w *softwarePWMWorker) repositionElement(elem *list.Element, newDeadline time.Time) {
	for e := elem.Next(); e != nil; e = e.Next() {
		p := e.Value.(*softPWMPin)
		eDeadline := p.deadline

		if newDeadline.Before(eDeadline) {
			if elem.Next() != e {
				w.pins.MoveBefore(elem, e)
			}
			return
		}
	}
	if elem != w.pins.Back() {
		w.pins.MoveToBack(elem)
	}
}

func (w *softwarePWMWorker) addPinInternal(pin *GPIOPin, freqHz, dutyCyclePct float64) {
	for e := w.pins.Front(); e != nil; e = e.Next() {
		p := e.Value.(*softPWMPin)
		if p.pin == pin {
			// Update existing pin's parameters
			p.freqHz = freqHz
			p.dutyCyclePct = dutyCyclePct
			p.calculateTimes()
			return
		}
	}

	pwmPin := &softPWMPin{
		pin:          pin,
		freqHz:       freqHz,
		dutyCyclePct: dutyCyclePct,
	}
	pwmPin.isOn = false
	pwmPin.calculateTimes()

	func() {
		for e := w.pins.Front(); e != nil; e = e.Next() {
			p := e.Value.(*softPWMPin)
			if pwmPin.deadline.Before(p.deadline) {
				w.pins.InsertBefore(pwmPin, e)
				return
			}
		}
		w.pins.PushBack(pwmPin)
	}()

	w.count.Add(1)
}

func (w *softwarePWMWorker) removePinInternal(pin *GPIOPin) {
	for e := w.pins.Front(); e != nil; e = e.Next() {
		p := e.Value.(*softPWMPin)
		if p.pin == pin {
			w.pins.Remove(e)
			w.count.Add(-1)
			break
		}
	}
}

// accurateSleep is intended to be a replacement for utils.SelectContextOrWait which wakes up
// closer to when it's supposed to. We return whether the context is still valid (not yet
// cancelled).
func accurateSleep(ctx context.Context, duration time.Duration) bool {
	// If we use utils.SelectContextOrWait(), we will wake up sometime after when we're supposed
	// to, which can be hundreds of microseconds later (because the process scheduler in the OS only
	// schedules things every millisecond or two). For use cases like a web server responding to a
	// query, that's fine. but when outputting a PWM signal, hundreds of microseconds can be a big
	// deal. To avoid this, we sleep for less time than we're supposed to, and then busy-wait until
	// the right time. Inspiration for this approach was taken from
	// https://blog.bearcats.nl/accurate-sleep-function/
	// On a raspberry pi 4, naively calling utils.SelectContextOrWait tended to have an error of
	// about 140-300 microseconds, while this version had an error of 0.3-0.6 microseconds.
	startTime := time.Now()
	maxBusyWaitTime := 1500 * time.Microsecond
	if duration > maxBusyWaitTime {
		shorterDuration := duration - maxBusyWaitTime
		if !utils.SelectContextOrWait(ctx, shorterDuration) {
			return false
		}
	}

	for time.Since(startTime) < duration {
		if err := ctx.Err(); err != nil {
			return false
		}
		// Otherwise, busy-wait some more
	}
	return true
}

// AddPin adds a pin to be managed by software pwm worker.
func (w *softwarePWMWorker) AddPin(pin *GPIOPin, freqHz, dutyCyclePct float64) error {
	if pin == nil {
		return errors.New("pin cannot be nil")
	}
	if freqHz <= 0.0 {
		return fmt.Errorf("frequency should be greater than 0.0 it is : %.3f", freqHz)
	}
	if dutyCyclePct > 1.0 || dutyCyclePct < 0.0 {
		return fmt.Errorf("duty cycle should be between 0.0 and 1.0 it is %.3f", dutyCyclePct)
	}

	op := &pwmOp{
		opType:       opAddPin,
		pin:          pin,
		freqHz:       freqHz,
		dutyCyclePct: dutyCyclePct,
	}

	w.opChan <- op
	return nil
}

// RemovePin removes a pin from software pwm worker.
func (w *softwarePWMWorker) RemovePin(pin *GPIOPin) error {
	if pin == nil {
		return errors.New("pin cannot be nil")
	}

	op := &pwmOp{
		opType: opRemovePin,
		pin:    pin,
	}

	w.opChan <- op
	return nil
}

// Clear stops the software pwm worker and removes all pins, returning close error for pending ops.
func (w *softwarePWMWorker) Clear() {
	if w.worker != nil {
		w.worker.Stop()
	}

	func() {
		for {
			select {
			case <-w.opChan:
				continue
			default:
				return
			}
		}
	}()

	w.pins.Init()
	w.count.Store(0)
}

// Stop stops the worker and cleans up resources.
func (w *softwarePWMWorker) Stop() {
	w.Clear()
}

// Count returns the number of active pins.
func (w *softwarePWMWorker) Count() int32 {
	return w.count.Load()
}
