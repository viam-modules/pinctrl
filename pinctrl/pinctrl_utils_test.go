//go:build linux

package pinctrl

import (
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/test"
)

// Test pinctrl_utils.go helper functions:.
func TestFindPathFromAlias(t *testing.T) {
	logger := logging.NewTestLogger(t)

	pinctrlCfg := Config{
		GPIOChipPath: "gpio0", DevMemPath: "/dev/gpiomem0", UseAlias: true,
		ChipSize: 0x30000, TestPath: "./mock-device-tree", UseGPIOMem: true,
	}

	boardPinCtrl, err := SetupPinControl(pinctrlCfg, logger)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, boardPinCtrl, test.ShouldNotBeNil)

	path := "/axi/pcie@120000/rp1/gpio@d0000"
	alias, err := findPathFromAlias("gpio0", pinctrlCfg.getBaseNodePath())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, cleanFilePath(alias), test.ShouldEqual, cleanFilePath(path))
	test.That(t, boardPinCtrl.Close(), test.ShouldBeNil)
}

func TestSetupNoAlias(t *testing.T) {
	logger := logging.NewTestLogger(t)

	pinctrlCfg := Config{
		GPIOChipPath: "/axi/pcie@120000/rp1/gpio@d0000", DevMemPath: "/dev/gpiomem0", UseAlias: false,
		ChipSize: 0x30000, TestPath: "./mock-device-tree", UseGPIOMem: true,
	}

	boardPinCtrl, err := SetupPinControl(pinctrlCfg, logger)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, boardPinCtrl, test.ShouldNotBeNil)
	test.That(t, boardPinCtrl.Close(), test.ShouldBeNil)
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
