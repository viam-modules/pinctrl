// //go:build linux

// // These tests will only run on Linux! Viam's automated build system on Github uses Linux, though,
// // so they should run on every PR. We made the tests Linux-only because this entire package is
// // Linux-only, and building non-Linux support solely for the test meant that the code tested might
// // not be the production code.
// package pi5

// import (
// 	"context"
// 	"testing"

// 	"go.viam.com/test"

// 	"go.viam.com/rdk/components/board"
// 	"go.viam.com/rdk/components/board/mcp3008helper"
// 	"go.viam.com/rdk/logging"
// 	"go.viam.com/rdk/resource"
// )

// func TestGenericLinux(t *testing.T) {
// 	ctx := context.Background()

// 	b := &Board{
// 		logger: logging.NewTestLogger(t),
// 	}

// 	t.Run("test empty sysfs board", func(t *testing.T) {
// 		_, err := b.GPIOPinByName("10")
// 		test.That(t, err, test.ShouldNotBeNil)
// 	})

// 	b = &Board{
// 		Named:         board.Named("foo").AsNamed(),
// 		gpioMappings:  nil,
// 		logger:        logging.NewTestLogger(t),
// 		cancelCtx:     ctx,
// 		cancelFunc: func() {
// 		},
// 	}

// 	t.Run("test digital-interrupts and gpio names", func(t *testing.T) {

// 		dns := b.DigitalInterruptNames()
// 		test.That(t, dns, test.ShouldBeNil)

// 		dn1, err := b.DigitalInterruptByName("dn")
// 		test.That(t, dn1, test.ShouldBeNil)
// 		test.That(t, err, test.ShouldNotBeNil)

// 		gn1, err := b.GPIOPinByName("10")
// 		test.That(t, err, test.ShouldNotBeNil)
// 		test.That(t, gn1, test.ShouldBeNil)
// 	})
// }

// func TestConfigValidate(t *testing.T) {
// 	validConfig := Config{}

// 	validConfig.DigitalInterrupts = []board.DigitalInterruptConfig{{}}
// 	_, err = validConfig.Validate("path")
// 	test.That(t, err, test.ShouldNotBeNil)
// 	test.That(t, err.Error(), test.ShouldContainSubstring, `path.digital_interrupts.0`)
// 	test.That(t, resource.GetFieldFromFieldRequiredError(err), test.ShouldEqual, "name")

// 	validConfig.DigitalInterrupts = []board.DigitalInterruptConfig{{Name: "bar"}}
// 	_, err = validConfig.Validate("path")
// 	test.That(t, err, test.ShouldNotBeNil)
// 	test.That(t, err.Error(), test.ShouldContainSubstring, `path.digital_interrupts.0`)
// 	test.That(t, resource.GetFieldFromFieldRequiredError(err), test.ShouldEqual, "pin")

// 	validConfig.DigitalInterrupts = []board.DigitalInterruptConfig{{Name: "bar", Pin: "3"}}
// 	_, err = validConfig.Validate("path")
// 	test.That(t, err, test.ShouldBeNil)
// }

// func TestNewBoard(t *testing.T) {
// 	logger := logging.NewTestLogger(t)
// 	ctx := context.Background()

// 	// Create a fake board mapping with two pins for testing.
// 	testBoardMappings := make(map[string]GPIOBoardMapping, 2)
// 	testBoardMappings["1"] = GPIOBoardMapping{
// 		GPIOChipDev:    "gpiochip0",
// 		GPIO:           1,
// 		GPIOName:       "1",
// 		PWMSysFsDir:    "",
// 		PWMID:          -1,
// 		HWPWMSupported: false,
// 	}
// 	testBoardMappings["2"] = GPIOBoardMapping{
// 		GPIOChipDev:    "gpiochip0",
// 		GPIO:           2,
// 		GPIOName:       "2",
// 		PWMSysFsDir:    "pwm.00",
// 		PWMID:          1,
// 		HWPWMSupported: true,
// 	}

// 	conf := &Config{}

// 	config := resource.Config{
// 		Name:                "board1",
// 		ConvertedAttributes: conf,
// 	}
// 	b, err := NewBoard(ctx, config, ConstPinDefs(testBoardMappings), logger)
// 	test.That(t, err, test.ShouldBeNil)
// 	test.That(t, b, test.ShouldNotBeNil)
// 	defer b.Close(ctx)

// 	dis := b.DigitalInterruptNames()
// 	test.That(t, dis, test.ShouldResemble, []string{})

// 	gn1, err := b.GPIOPinByName("1")
// 	test.That(t, err, test.ShouldBeNil)
// 	test.That(t, gn1, test.ShouldNotBeNil)

//		gn2, err := b.GPIOPinByName("2")
//		test.That(t, err, test.ShouldBeNil)
//		test.That(t, gn2, test.ShouldNotBeNil)
//	}
package pi5

import (
	"fmt"
)

func test() {
	fmt.Printf("hello testing\n")
}
