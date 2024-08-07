//go:build linux

package pi5

import (
	"context"
	"testing"

	"go.viam.com/test"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

func TestEmptyBoard(t *testing.T) {
	b := &pinctrlpi5{
		logger: logging.NewTestLogger(t),
	}

	t.Run("test empty sysfs board", func(t *testing.T) {
		_, err := b.GPIOPinByName("10")
		test.That(t, err, test.ShouldNotBeNil)
	})
}

func TestNewBoard(t *testing.T) {
	logger := logging.NewTestLogger(t)
	ctx := context.Background()

	// Create a fake board mapping with two pins for testing.
	// BoardMappings are needed as a parameter passed in to NewBoard but are not used for pin control testing yet.
	testBoardMappings := make(map[string]GPIOBoardMapping, 0)
	conf := &Config{}
	config := resource.Config{
		Name:                "board1",
		ConvertedAttributes: conf,
	}

	// Test Creations of Boards
	newB, err := newBoard(ctx, config, ConstPinDefs(testBoardMappings), logger, true)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, newB, test.ShouldNotBeNil)
	defer newB.Close(ctx)

	// Cast from board.Board to pinctrlpi5 is required to access board's vars
	p5 := newB.(*pinctrlpi5)
	test.That(t, p5.chipSize, test.ShouldEqual, 0x30000)
	test.That(t, p5.physAddr, test.ShouldEqual, 0x1f000d0000)
}

// Test pinctrl_utils.go helper functions:
func TestFindPathFromAlias(t *testing.T) {
	logger := logging.NewTestLogger(t)
	ctx := context.Background()

	// Create a fake empty board mapping.
	// These are needed for making board; not actually used for pin control testing yet.
	testBoardMappings := make(map[string]GPIOBoardMapping, 0)

	conf := &Config{}
	config := resource.Config{
		Name:                "board1",
		ConvertedAttributes: conf,
	}
	newB, err := newBoard(ctx, config, ConstPinDefs(testBoardMappings), logger, true)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, newB, test.ShouldNotBeNil)
	defer newB.Close(ctx)

	// Cast from board.Board to pinctrlpi5 is required to access board's vars
	p5 := newB.(*pinctrlpi5)

	path := "/axi/pcie@120000/rp1/gpio@d0000"
	alias, err := p5.findPathFromAlias("gpio0")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, cleanFilePath(alias), test.ShouldEqual, cleanFilePath(path))
}

func TestParseCells(t *testing.T) {
	byteArray := []byte{0x01, 0x02, 0x03, 0x04}
	val, err := parseCells(0, &byteArray)
	test.That(t, err.Error(), test.ShouldContainSubstring, "attempting to read <1 cells: num was")
	test.That(t, val, test.ShouldEqual, 0x0)

	val, err = parseCells(1, &byteArray)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, val, test.ShouldEqual, 0x1020304)

	val, err = parseCells(2, &byteArray)
	test.That(t, err.Error(), test.ShouldContainSubstring, "but there aren't enough bytes to read in from inputted bytestream")
	test.That(t, val, test.ShouldEqual, 0x0)

	byteArray = []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	val, err = parseCells(2, &byteArray)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, val, test.ShouldEqual, 0x102030405060708)

	// this parseCells() call should omit the first 32 bits from the final outputted value
	byteArray = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	val, err = parseCells(3, &byteArray)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, val, test.ShouldEqual, 0x102030405060708)
}

func TestGetAddrSizeCells(t *testing.T) {
	// this should throw an error since there are no address/size cell files here
	path := "./mock-device-tree/proc/device-tree/axi/pcie@120000/rp1/gpio@d0000"
	addr, size, err := getNumAddrSizeCells(path)
	test.That(t, err.Error(), test.ShouldContainSubstring, "trouble getting addr cells info for")
	test.That(t, addr, test.ShouldEqual, 0)
	test.That(t, size, test.ShouldEqual, 0)

	path = "./mock-device-tree/proc/device-tree/axi/pcie@120000/rp1"
	addr, size, err = getNumAddrSizeCells(path)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, addr, test.ShouldEqual, 2)
	test.That(t, size, test.ShouldEqual, 2)

	path = "./mock-device-tree/proc/device-tree/axi/pcie@120000"
	addr, size, err = getNumAddrSizeCells(path)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, addr, test.ShouldEqual, 3)
	test.That(t, size, test.ShouldEqual, 2)
}

func TestGetRegAddr(t *testing.T) {
	path := "./mock-device-tree/proc/device-tree/axi/pcie@120000/rp1/gpio@d0000"
	reg, err := getRegAddr(path, 2)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, reg, test.ShouldEqual, 0xC0400D0000)
}

func TestGetRangesAddr(t *testing.T) {
	path := "./mock-device-tree/proc/device-tree/axi/pcie@120000/rp1"
	ranges, err := getRangesAddr(path, 3, 2, 2)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, ranges[0].childAddr, test.ShouldEqual, 0x4000000002000000)
	test.That(t, ranges[0].parentAddr, test.ShouldEqual, 0x0)
	test.That(t, ranges[0].addrSpaceSize, test.ShouldEqual, 0x400000)

	path = "./mock-device-tree/proc/device-tree/axi/pcie@120000"
	ranges, err = getRangesAddr(path, 3, 2, 2)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, ranges[0].childAddr, test.ShouldEqual, 0x0)
	test.That(t, ranges[0].parentAddr, test.ShouldEqual, 0x1F00000000)
	test.That(t, ranges[0].addrSpaceSize, test.ShouldEqual, 0xFFFFFFFC)
	test.That(t, ranges[1].childAddr, test.ShouldEqual, 0x400000000)
	test.That(t, ranges[1].parentAddr, test.ShouldEqual, 0x1C00000000)
	test.That(t, ranges[1].addrSpaceSize, test.ShouldEqual, 0x300000000)
}

func TestDeviceTreeParsing(t *testing.T) {
	t.Run("test board setup", func(t *testing.T) {
		TestEmptyBoard(t)
		TestNewBoard(t)
	})
	t.Run("test device tree parsing on mock tree", func(t *testing.T) {
		TestFindPathFromAlias(t)
		TestParseCells(t)
		TestGetAddrSizeCells(t)
		TestGetRegAddr(t)
		TestGetRangesAddr(t)
	})
}
