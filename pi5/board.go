//go:build linux

package pi5

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/multierr"
	pb "go.viam.com/api/component/board/v1"
	"go.viam.com/utils"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/board/genericlinux/buses"
	"go.viam.com/rdk/components/board/mcp3008helper"
	"go.viam.com/rdk/components/board/pinwrappers"
	"go.viam.com/rdk/grpc"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

const dtBaseNodePath = "/proc/device-tree"

type rangeInfo struct {
	childAddr     uint64
	parentAddr    uint64
	addrSpaceSize uint64
}

type gpioChip struct {
	name     string
	dtNode   string
	physAddr uint64
	virtAddr uint64
	chipSize uint32
	// memfd     uint32
}

var INVALID_ADDR uint64 = math.MaxUint64

var (
	Model = resource.NewModel("viam-labs", "pinctrl", "pi5")
)

func init() {
	gpioMappings, err := GetGPIOBoardMappings(Model.Name, boardInfoMappings)
	var noBoardErr NoBoardFoundError
	if errors.As(err, &noBoardErr) {
		logging.Global().Debugw("Error getting raspi5 GPIO board mapping", "error", err)
	}

	RegisterBoard(Model.Name, gpioMappings)
}

// Sets up GPIO Pin Memory Access by parsing the device tree for relevant address information
func (b *pinctrlpi5) pinControlSetup() error {
	nodePath, err := b.findGPIONodenodePath("gpio0") // this ("gpio") is hardcoded now, we will fix that later!
	if nodePath == "NULL" || err != nil {
		logging.Global().Debugw("error getting raspi5 GPIO nodePath", "error", err)
	}

	err = b.setGPIONodePhysAddr(nodePath)
	if err != nil {
		logging.Global().Debugw("error getting raspi5 GPIO physical address", "error", err)
	}
	return err
}

// We look in the 'aliases' node at the base of proc/device-tree to determine the full file path required to access our GPIO Chip
func (b *pinctrlpi5) findGPIONodenodePath(nodeName string) (string, error) {

	dtNodePath := dtBaseNodePath + "/aliases/" + nodeName
	nodePathBytes, err := os.ReadFile(dtNodePath)
	if err != nil {
		return "NULL", fmt.Errorf("Error reading directory: %w", err)
	}

	// convert readFile output from bytes -> string format
	nodePath := fmt.Sprintf("%s", nodePathBytes)
	return nodePath, err
}

/*
A child Node will call this method ON ITS PARENT to determine how many cells denote parent address, child address, addr space size when
reading its ranges or reg properties.

returns:

	numPAddrCells:	number of 32 bit chunks needed to represent the parent address
	numAddrSpaceCells:	number of 32 bit chunks needed to represent the size of the parent address space
*/
func getNumAddrSizeCellsInfo(parentNodePath string) (uint32, uint32, error) {

	// get #address - cells info for child node using the parent Node
	npaByteContents, err := os.ReadFile(parentNodePath + "/#address-cells")
	if err != nil {
		return 0, 0, fmt.Errorf("trouble getting addr cells info for %s: %w\n", parentNodePath, err)
	}
	numPAddrCells := binary.BigEndian.Uint32(npaByteContents[:4])

	// get #size - cells info for child node using the parent Node
	npsByteContents, err := os.ReadFile(parentNodePath + "/#size-cells")
	if err != nil {
		return 0, 0, fmt.Errorf("trouble getting size cells info for %s: %w\n", parentNodePath, err)
	}
	numAddrSpaceCells := binary.BigEndian.Uint32(npsByteContents[:4]) // reading 4 bytes because the number is represented by 1 uint32. 4bytes * 8bits/byte = 32 bits
	return numPAddrCells, numAddrSpaceCells, err
}

// Reads the /reg file and converts the bytestream into a uint64 representing the GPIO Node's physical address within its parent's space. (pre address mapping)
func getRegAddr(childNodePath string, numPAddrCells uint32) (uint64, error) {

	//newPath := "/proc/device-tree/axi/pcie@120000/rp1/gpio@d0000/reg"

	childNodePath += "/reg"
	childNodePath = strings.TrimSpace(childNodePath)

	re := regexp.MustCompile(`[\x00-\x1F\x7F-\x9F]`) // gets rid of random non printable chars. works for now but make cleaner later
	childNodePath = re.ReplaceAllString(childNodePath, "")

	regByteContents, err := os.ReadFile(childNodePath)
	if err != nil {
		return INVALID_ADDR, fmt.Errorf("trouble getting reg addr info for %s: %w\n", childNodePath, err)
	}

	physAddr := INVALID_ADDR
	if numPAddrCells == 1 { // reading in 32 bits. regardless we must convert to a 64 bit address so we add a bunch of 0s to the beginning.
		physAddr = uint64(binary.BigEndian.Uint32(regByteContents[:(4 * numPAddrCells)]))
	} else if numPAddrCells == 2 { // reading in 64 bits
		physAddr = binary.BigEndian.Uint64(regByteContents[:(4 * numPAddrCells)])
	} else { // reading in more than 64 bits. we only want the last 64 bits of the address though, so we cut off the other portion of it
		physAddr = binary.BigEndian.Uint64(regByteContents[(4 * (numPAddrCells - 2)):(4 * numPAddrCells)])
	}
	return physAddr, err
}

// Reads the /ranges file and converts the bytestream into integers representing the < child address parent address parent size >
func getRangesAddrInfo(childNodePath string, numCAddrCells uint32, numPAddrCells uint32, numAddrSpaceCells uint32) ([]rangeInfo, error) {

	childNodePath += "/ranges"
	childNodePath = strings.TrimSpace(childNodePath)
	re := regexp.MustCompile(`[\x00-\x1F\x7F-\x9F]`) // gets rid of random non printable chars. works for now but make cleaner later. remvonig the -9F causes bugs.
	childNodePath = re.ReplaceAllString(childNodePath, "")
	var addrRangesSlice []rangeInfo

	rangeByteContents, err := os.ReadFile(childNodePath)
	if err != nil {
		return addrRangesSlice, fmt.Errorf("trouble getting reg addr info for %s: %w\n", childNodePath, err)
	}

	numRanges := uint32(len(rangeByteContents)) / (4 * (numCAddrCells + numPAddrCells + numAddrSpaceCells))

	for i := uint32(0); i < numRanges; i++ {

		childAddr, parentAddr := INVALID_ADDR, INVALID_ADDR
		addrSpaceSize := uint64(0)

		if numCAddrCells == 1 { // reading in 32 bits. regardless we must convert to a 64 bit address so we add a bunch of 0s to the beginning.
			childAddr = uint64(binary.BigEndian.Uint32(rangeByteContents[:(4 * numCAddrCells)]))
		} else if numCAddrCells == 2 { // reading in 64 bits
			childAddr = binary.BigEndian.Uint64(rangeByteContents[:(4 * numCAddrCells)])
		} else { // reading in more than 64 bits. we only want the last 64 bits of the address though, so we cut off the other portion of it
			childAddr = binary.BigEndian.Uint64(rangeByteContents[(4 * (numCAddrCells - 2)):(4 * numCAddrCells)])
		}
		rangeByteContents = rangeByteContents[(4 * numCAddrCells):] // flush the bytes already parsed out of the array

		if numPAddrCells == 1 { // reading in 32 bits. regardless we must convert to a 64 bit address so we add a bunch of 0s to the beginning.
			parentAddr = uint64(binary.BigEndian.Uint32(rangeByteContents[:(4 * numPAddrCells)]))
		} else if numPAddrCells == 2 { // reading in 64 bits
			parentAddr = binary.BigEndian.Uint64(rangeByteContents[:(4 * numPAddrCells)])
		} else { // reading in more than 64 bits. we only want the last 64 bits of the address though, so we cut off the other portion of it
			parentAddr = binary.BigEndian.Uint64(rangeByteContents[(4 * (numPAddrCells - 2)):(4 * numPAddrCells)])
		}
		rangeByteContents = rangeByteContents[(4 * numPAddrCells):]

		if numAddrSpaceCells == 1 { // reading in 32 bits. regardless we must convert to a 64 bit address so we add a bunch of 0s to the beginning.
			addrSpaceSize = uint64(binary.BigEndian.Uint32(rangeByteContents[:(4 * numAddrSpaceCells)]))
		} else if numAddrSpaceCells == 2 { // reading in 64 bits
			addrSpaceSize = binary.BigEndian.Uint64(rangeByteContents[:(4 * numAddrSpaceCells)])
		} else { // reading in more than 64 bits. we only want the last 64 bits of the address though, so we cut off the other portion of it
			addrSpaceSize = binary.BigEndian.Uint64(rangeByteContents[(4 * (numAddrSpaceCells - 2)):(4 * numAddrSpaceCells)])
		}

		rangeByteContents = rangeByteContents[(4 * numAddrSpaceCells):]
		rangeInfo := rangeInfo{childAddr: childAddr, parentAddr: parentAddr, addrSpaceSize: addrSpaceSize}
		addrRangesSlice = append(addrRangesSlice, rangeInfo)

	}

	return addrRangesSlice, err
}

// Uses Information Stored within the 'reg' property of the child node and 'ranges' property of its parents to map the child's physical address into the dev/gpiomem space
func (b *pinctrlpi5) setGPIONodePhysAddr(nodePath string) error {

	currNodePath := dtBaseNodePath + nodePath // initially: /proc/device-tree/axi/pcie@120000/rp1/gpio@d0000
	var numCAddrCells uint32 = 0
	var err error = nil

	/* Call Recursive Function to Calculate Phys Addr. Works way up the Device Tree, using the information
	found in #ranges at every node to translate from the child's address space to the parent's address space
	until we get the child's physical address in all of /dev/gpiomem. */
	*b.physAddr, err = setGPIONodePhysAddrHelper(currNodePath, INVALID_ADDR, numCAddrCells)
	if err != nil {
		return fmt.Errorf("trouble calculating phys addr for %s: %w\n", nodePath, err)
	}

	return nil
}

// Recursively Traverses Device Tree to Calcuate Physical Address of specified GPIO Chip
func setGPIONodePhysAddrHelper(currNodePath string, physAddress uint64, numCAddrCells uint32) (uint64, error) {

	if currNodePath == dtBaseNodePath { // Base Case: We are at the root of the device tree.
		return physAddress, nil
	}

	// Normal Case: We are not at the root of the device tree. We must continue mapping our child addr (from the previous call) to this parent's addr space.
	parentNodePath := filepath.Dir(currNodePath)
	numPAddrCells, numAddrSpaceCells, err := getNumAddrSizeCellsInfo(parentNodePath)
	if err != nil {
		return INVALID_ADDR, err
	}

	var addrRangesSlice []rangeInfo
	if physAddress == INVALID_ADDR { // Case 1: We are the Child Node. No addr has been set. Read the reg file to get the physical address within our parents space.
		physAddress, err = getRegAddr(currNodePath, numPAddrCells)
		if err != nil {
			return INVALID_ADDR, err
		}

	} else { // Case 2: We use the ranges property to continue mapping a child addr into our parent addr space.
		addrRangesSlice, err = getRangesAddrInfo(currNodePath, numCAddrCells, numPAddrCells, numAddrSpaceCells)
		if err != nil {
			return INVALID_ADDR, err
		}

		for _, addrRange := range addrRangesSlice {

			if addrRange.childAddr <= physAddress && physAddress <= addrRange.childAddr+addrRange.addrSpaceSize {
				physAddress -= addrRange.childAddr  // get the offset beween the address and child base address
				physAddress += addrRange.parentAddr // now address has been mapped into parent space.
				break
			}
		}
	}

	numCAddrCells = numPAddrCells
	currNodePath = parentNodePath
	return setGPIONodePhysAddrHelper(currNodePath, physAddress, numCAddrCells)
}

// RegisterBoard registers a sysfs based board of the given model.
// using this constructor to pass in the GPIO mappings
func RegisterBoard(modelName string, gpioMappings map[string]GPIOBoardMapping) {
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
				return newBoard(ctx, conf, ConstPinDefs(gpioMappings), logger)
			},
		})
}

// NewBoard is the constructor for a Board.
func newBoard(
	ctx context.Context,
	conf resource.Config,
	convertConfig ConfigConverter,
	logger logging.Logger,
) (board.Board, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	var physical_address uint64 = 0
	var virtual_address uint64 = 0
	var gpioMemIdx [4]rune

	b := &pinctrlpi5{
		Named:         conf.ResourceName().AsNamed(),
		convertConfig: convertConfig,

		logger:     logger,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,

		analogReaders: map[string]*wrappedAnalogReader{},
		gpios:         map[string]*gpioPin{},
		interrupts:    map[string]*digitalInterrupt{},

		// store addresses + other stuff here
		gpioNodePath: "",
		memFD:        0,
		physAddr:     &physical_address,
		virtAddr:     &virtual_address,
		gpioMemIdx:   gpioMemIdx,
	}
	if err := b.Reconfigure(cancelCtx, nil, conf); err != nil {
		return nil, err
	}

	if err := b.pinControlSetup(); err != nil {
		return nil, err
	}
	return b, nil
}

// Reconfigure reconfigures the board with interrupt pins, spi and i2c, and analogs.
func (b *pinctrlpi5) Reconfigure(
	ctx context.Context,
	_ resource.Dependencies,
	conf resource.Config,
) error {
	newConf, err := b.convertConfig(conf, b.logger)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.reconfigureGpios(newConf); err != nil {
		return err
	}
	if err := b.reconfigureAnalogReaders(ctx, newConf); err != nil {
		return err
	}
	if err := b.reconfigureInterrupts(newConf); err != nil {
		return err
	}
	return nil
}

// This is a helper function used to reconfigure the GPIO pins. It looks for the key in the map
// whose value resembles the target pin definition.
func getMatchingPin(target GPIOBoardMapping, mapping map[string]GPIOBoardMapping) (string, bool) {
	for name, def := range mapping {
		if target == def {
			return name, true
		}
	}
	return "", false
}

func (b *pinctrlpi5) reconfigureGpios(newConf *LinuxBoardConfig) error {
	// First, find old pins that are no longer defined, and destroy them.
	for oldName, mapping := range b.gpioMappings {
		if _, ok := getMatchingPin(mapping, newConf.GpioMappings); ok {
			continue // This pin is in the new mapping, so don't destroy it.
		}

		// Otherwise, remove the pin because it's not in the new mapping.
		if pin, ok := b.gpios[oldName]; ok {
			if err := pin.Close(); err != nil {
				return err
			}
			delete(b.gpios, oldName)
			continue
		}

		// If we get here, the old pin definition exists, but the old pin does not. Check if it's a
		// digital interrupt.
		if interrupt, ok := b.interrupts[oldName]; ok {
			if err := interrupt.Close(); err != nil {
				return err
			}
			delete(b.interrupts, oldName)
			continue
		}

		// If we get here, there is a logic bug somewhere. but failing to delete a nonexistent pin
		// seemingly doesn't hurt anything, so just log the error and continue.
		b.logger.Errorf("During reconfiguration, old pin '%s' should be destroyed, but "+
			"it doesn't exist!?", oldName)
	}

	// Next, compare the new pin definitions to the old ones, to build up 2 sets: pins to rename,
	// and new pins to create. Don't actually create any yet, in case you'd overwrite a pin that
	// should be renamed out of the way first.
	toRename := map[string]string{} // Maps old names for pins to new names
	toCreate := map[string]GPIOBoardMapping{}
	for newName, mapping := range newConf.GpioMappings {
		if oldName, ok := getMatchingPin(mapping, b.gpioMappings); ok {
			if oldName != newName {
				toRename[oldName] = newName
			}
		} else {
			toCreate[newName] = mapping
		}
	}

	// Rename the ones whose name changed. The ordering here is tricky: if B should be renamed to C
	// while A should be renamed to B, we need to make sure we don't overwrite B with A and then
	// rename it to C. To avoid this, move all the pins to rename into a temporary data structure,
	// then move them all back again afterward.
	tempGpios := map[string]*gpioPin{}
	tempInterrupts := map[string]*digitalInterrupt{}
	for oldName, newName := range toRename {
		if pin, ok := b.gpios[oldName]; ok {
			tempGpios[newName] = pin
			delete(b.gpios, oldName)
			continue
		}

		// If we get here, again check if the missing pin is a digital interrupt.
		if interrupt, ok := b.interrupts[oldName]; ok {
			tempInterrupts[newName] = interrupt
			delete(b.interrupts, oldName)
			continue
		}

		return fmt.Errorf("during reconfiguration, old pin '%s' should be renamed to '%s', but "+
			"it doesn't exist!?", oldName, newName)
	}

	// Now move all the pins back from the temporary data structures.
	for newName, pin := range tempGpios {
		b.gpios[newName] = pin
	}
	for newName, interrupt := range tempInterrupts {
		b.interrupts[newName] = interrupt
	}

	// Finally, create the new pins.
	for newName, mapping := range toCreate {
		b.gpios[newName] = b.createGpioPin(mapping)
	}

	b.gpioMappings = newConf.GpioMappings
	return nil
}

func (b *pinctrlpi5) reconfigureAnalogReaders(ctx context.Context, newConf *LinuxBoardConfig) error {
	stillExists := map[string]struct{}{}
	for _, c := range newConf.AnalogReaders {
		channel, err := strconv.Atoi(c.Pin)
		if err != nil {
			return errors.Errorf("bad analog pin (%s)", c.Pin)
		}

		bus := buses.NewSpiBus(c.SPIBus)

		stillExists[c.Name] = struct{}{}
		if curr, ok := b.analogReaders[c.Name]; ok {
			if curr.chipSelect != c.ChipSelect {
				ar := &mcp3008helper.MCP3008AnalogReader{channel, bus, c.ChipSelect}
				curr.reset(ctx, curr.chipSelect,
					pinwrappers.SmoothAnalogReader(ar, board.AnalogReaderConfig{
						AverageOverMillis: c.AverageOverMillis, SamplesPerSecond: c.SamplesPerSecond,
					}, b.logger))
			}
			continue
		}
		ar := &mcp3008helper.MCP3008AnalogReader{channel, bus, c.ChipSelect}
		b.analogReaders[c.Name] = newWrappedAnalogReader(ctx, c.ChipSelect,
			pinwrappers.SmoothAnalogReader(ar, board.AnalogReaderConfig{
				AverageOverMillis: c.AverageOverMillis, SamplesPerSecond: c.SamplesPerSecond,
			}, b.logger))
	}

	for name := range b.analogReaders {
		if _, ok := stillExists[name]; ok {
			continue
		}
		b.analogReaders[name].reset(ctx, "", nil)
		delete(b.analogReaders, name)
	}
	return nil
}

// This helper function is used while reconfiguring digital interrupts. It finds the new config (if
// any) for a pre-existing digital interrupt.
func findNewDigIntConfig(
	interrupt *digitalInterrupt, confs []board.DigitalInterruptConfig, logger logging.Logger,
) *board.DigitalInterruptConfig {
	for _, newConfig := range confs {
		if newConfig.Pin == interrupt.config.Pin {
			return &newConfig
		}
	}
	if interrupt.config.Name == interrupt.config.Pin {
		// This interrupt is named identically to its pin. It was probably created on the fly
		// by some other component (an encoder?). Unless there's now some other config with the
		// same name but on a different pin, keep it initialized as-is.
		for _, intConfig := range confs {
			if intConfig.Name == interrupt.config.Name {
				// The name of this interrupt is defined in the new config, but on a different
				// pin. This interrupt should be closed.
				return nil
			}
		}
		logger.Debugf(
			"Keeping digital interrupt on pin %s even though it's not explicitly mentioned "+
				"in the new board config",
			interrupt.config.Pin)
		return &interrupt.config
	}
	return nil
}

func (b *pinctrlpi5) reconfigureInterrupts(newConf *LinuxBoardConfig) error {
	// Any pin that already exists in the right configuration should just be copied over; closing
	// and re-opening it risks losing its state.
	newInterrupts := make(map[string]*digitalInterrupt, len(newConf.DigitalInterrupts))

	// Reuse any old interrupts that have new configs
	for _, oldInterrupt := range b.interrupts {
		if newConfig := findNewDigIntConfig(oldInterrupt, newConf.DigitalInterrupts, b.logger); newConfig == nil {
			// The old interrupt shouldn't exist any more, but it probably became a GPIO pin.
			if err := oldInterrupt.Close(); err != nil {
				return err
			}
			if newGpioConfig, ok := b.gpioMappings[oldInterrupt.config.Pin]; ok {
				b.gpios[oldInterrupt.config.Pin] = b.createGpioPin(newGpioConfig)
			} else {
				b.logger.Warnf("Old interrupt pin was on nonexistent GPIO pin '%s', ignoring",
					oldInterrupt.config.Pin)
			}
		} else { // The old interrupt should stick around.
			oldInterrupt.UpdateConfig(*newConfig)
			newInterrupts[newConfig.Name] = oldInterrupt
		}
	}
	oldInterrupts := b.interrupts
	b.interrupts = newInterrupts

	// Add any new interrupts that should be freshly made.
	for _, config := range newConf.DigitalInterrupts {
		if interrupt, ok := b.interrupts[config.Name]; ok {
			if interrupt.config.Pin == config.Pin {
				continue // Already initialized; keep going
			}
			// If the interrupt's name matches but the pin does not, the interrupt we already have
			// was implicitly created (e.g., its name is "38" so we created it on pin 38 even
			// though it was not explicitly mentioned in the old board config), but the new config
			// is explicit (e.g., its name is still "38" but it's been moved to pin 37). Close the
			// old one and initialize it anew.
			if err := interrupt.Close(); err != nil {
				return err
			}
			// Although we delete the implicit interrupt from b.interrupts, it's still in
			// oldInterrupts, so we haven't lost the channels it reports to and can still copy them
			// over to the new struct, if necessary.
			delete(b.interrupts, config.Name)
		}

		if oldPin, ok := b.gpios[config.Pin]; ok {
			if err := oldPin.Close(); err != nil {
				return err
			}
			delete(b.gpios, config.Pin)
		}

		// If there was an old interrupt pin with this same name, anything subscribed to the old
		// pin should still be subscribed to the new one.
		oldInterrupt, ok := oldInterrupts[config.Name]
		if !ok {
			oldInterrupt = nil
		}

		gpioMapping, ok := b.gpioMappings[config.Name]
		if !ok {
			return fmt.Errorf("cannot create digital interrupt on unknown pin %s", config.Name)
		}
		interrupt, err := newDigitalInterrupt(config, gpioMapping, oldInterrupt)
		if err != nil {
			return err
		}
		b.interrupts[config.Name] = interrupt
	}

	return nil
}

func (b *pinctrlpi5) createGpioPin(mapping GPIOBoardMapping) *gpioPin {
	pin := gpioPin{
		boardWorkers: &b.activeBackgroundWorkers,
		devicePath:   mapping.GPIOChipDev,
		offset:       uint32(mapping.GPIO),
		cancelCtx:    b.cancelCtx,
		logger:       b.logger,
	}
	if mapping.HWPWMSupported {
		pin.hwPwm = newPwmDevice(mapping.PWMSysFsDir, mapping.PWMID, b.logger)
	}
	return &pin
}

// Board implements a component for a Linux machine.
type pinctrlpi5 struct {
	resource.Named
	mu            sync.RWMutex
	convertConfig ConfigConverter

	gpioMappings  map[string]GPIOBoardMapping
	analogReaders map[string]*wrappedAnalogReader
	logger        logging.Logger

	gpios      map[string]*gpioPin
	interrupts map[string]*digitalInterrupt

	/* Custom PinCTRL Params Here: */
	dtBaseNodePath string
	memFD          int32
	virtAddr       *uint64
	physAddr       *uint64
	gpioMemIdx     [4]rune
	gpioNodePath   string

	cancelCtx               context.Context
	cancelFunc              func()
	activeBackgroundWorkers sync.WaitGroup
}

// AnalogByName returns the analog pin by the given name if it exists.
func (b *pinctrlpi5) AnalogByName(name string) (board.Analog, error) {
	a, ok := b.analogReaders[name]
	if !ok {
		return nil, errors.Errorf("can't find AnalogReader (%s)", name)
	}
	return a, nil
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
	names := []string{}
	for k := range b.analogReaders {
		names = append(names, k)
	}
	return names
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
	b.cancelFunc()
	b.mu.Unlock()
	b.activeBackgroundWorkers.Wait()

	var err error
	for _, pin := range b.gpios {
		err = multierr.Combine(err, pin.Close())
	}
	for _, interrupt := range b.interrupts {
		err = multierr.Combine(err, interrupt.Close())
	}
	for _, reader := range b.analogReaders {
		err = multierr.Combine(err, reader.Close(ctx))
	}
	return err
}
