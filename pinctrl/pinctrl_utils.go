//go:build linux

package pinctrl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	mmap "github.com/edsrzf/mmap-go"
	"go.viam.com/rdk/logging"
)

const dtBase = "/proc/device-tree"

/*
rangeInfo represents the info provided in the ranges property of a device tree.
It provides a mapping between addresses in the child address space to the parent
address space.

For example, if a range looks like:
< child  parent size  >
< 0x0000 0x4000 0x00CF>

Then we know that something in the child address space at address 0x0000 maps to
parent address space address 0x4000, and the size of the address space is 0x00CF.
For child address 0x00E1, its address in the parent's address space would be 0x40E1.
*/
type rangeInfo struct {
	childAddr     uint64
	parentAddr    uint64
	addrSpaceSize uint64
}

// Config is the config used to define the names for pinctrl on a board. These are needed to use pinctrl with a device.
type Config struct {
	GPIOChipPath string // path to the gpio chip in the device tree
	GPIOMemPath  string
	TestPath     string // path to a mock device tree to use in tests
	ChipSize     uint64 // length of chip's address space in memory
	UseAlias     bool   // if your board has an alias for the chip, you can use this instead
}

func (cfg *Config) getBaseNodePath() string {
	if cfg.TestPath != "" {
		return cfg.TestPath + dtBase
	}
	return dtBase
}

// Pinctrl defines the objects used when using pinctrl to control a board/device.
type Pinctrl struct {
	VirtAddr *byte     // base address of mapped virtual page referencing the gpio chip data
	PhysAddr uint64    // base address of the gpio chip data in /dev/mem/
	MemFile  *os.File  // actual file to open that the virtual page will point to. Need to keep track of this for cleanup
	VPage    mmap.MMap // virtual page pointing to dev/gpiomem's physical page in memory. Need to keep track of this for cleanup
	Cfg      Config
	logger   logging.Logger
}

// Cleans file path before opening files in device tree.
func cleanFilePath(nodePath string) string {
	nodePath = strings.TrimSpace(nodePath)
	re := regexp.MustCompile(`[\x00-\x1F]`) // gets rid of Null Chars & Non Printable Chars in File Path
	nodePath = re.ReplaceAllString(nodePath, "")
	nodePath = filepath.Clean(nodePath)
	return nodePath
}

// We look in the 'aliases' node at the base of proc/device-tree to determine the full file path required to access our GPIO Chip.
func findPathFromAlias(nodeName, dtBaseNodePath string) (string, error) {
	dtNodePath := dtBaseNodePath + "/aliases/" + nodeName
	//nolint:gosec
	nodePathBytes, err := os.ReadFile(dtNodePath)
	if err != nil {
		return "", fmt.Errorf("error reading directory: %w", err)
	}

	// convert readFile output from bytes -> string format
	nodePath := string(nodePathBytes)
	return nodePath, err
}

// Read in 'numCells' 32 bit chunks from byteContents, the bytestream outputted from reading the file '/ranges'.
// Convert bytes into their uint64 value
// Note that 1 cell is 32 bits, (4 bytes).  numCells is multiplied by 4 to retrieve all 4 bytes associated with the cell.
func parseCells(numCells uint32, byteContents *[]byte) (uint64, error) {
	var parsedValue uint64

	if numCells < 1 {
		return 0, fmt.Errorf("attempting to read <1 cells: num was %d", numCells)
	}

	if len(*byteContents) < int(numCells)*4 {
		return 0,
			fmt.Errorf("num cells was: %d, but there aren't enough bytes to read in from inputted bytestream (only %d)",
				numCells, len(*byteContents))
	}

	switch numCells {
	// reading in 32 bits. regardless we must convert to a 64 bit address so we add a bunch of 0s to the beginning.
	case 1:
		parsedValue = uint64(binary.BigEndian.Uint32((*byteContents)[:4]))

	// reading in 64 bits
	case 2:
		parsedValue = binary.BigEndian.Uint64((*byteContents)[:(4 * numCells)])

	// reading in more than 64 bits. we only want the last 64 bits (2 cells) of the address so we cut off the other portion
	default:
		parsedValue = binary.BigEndian.Uint64((*byteContents)[(4 * (numCells - 2)):(4 * numCells)])
	}

	*byteContents = (*byteContents)[(4 * numCells):] // flush the bytes already parsed out of the array
	return parsedValue, nil
}

/*
A child Node will call this method ON ITS PARENT to determine how many cells denote parent address, child address, addr space size when
reading its ranges or reg properties.

returns:

	numPAddrCells:	number of 32 bit chunks needed to represent the parent address
	numAddrSpaceCells:	number of 32 bit chunks needed to represent the size of the parent address space
*/
func getNumAddrSizeCells(parentNodePath string) (uint32, uint32, error) {
	// get #address - cells info for child node using the parent Node
	//nolint:gosec
	npaByteContents, err := os.ReadFile(parentNodePath + "/#address-cells")
	if err != nil {
		return 0, 0, fmt.Errorf("trouble getting addr cells info for %s: %w", parentNodePath, err)
	}
	numPAddrCells := binary.BigEndian.Uint32(npaByteContents[:4])

	// get #size - cells info for child node using the parent Node
	//nolint:gosec
	npsByteContents, err := os.ReadFile(parentNodePath + "/#size-cells")
	if err != nil {
		return 0, 0, fmt.Errorf("trouble getting size cells info for %s: %w", parentNodePath, err)
	}
	// reading 4 bytes because the number is represented by 1 uint32. 4bytes * 8bits/byte = 32 bits
	numAddrSpaceCells := binary.BigEndian.Uint32(npsByteContents[:4])

	return numPAddrCells, numAddrSpaceCells, err
}

// Reads the /reg file and converts the bytestream into a uint64 representing
// the GPIO Node's physical address within its parent's space. (pre address mapping).
func getRegAddr(childNodePath string, numPAddrCells uint32) (uint64, error) {
	childNodePath += "/reg"
	childNodePath = cleanFilePath(childNodePath)
	invalidAddr := uint64(math.NaN())

	//nolint:gosec
	regByteContents, err := os.ReadFile(childNodePath)
	if err != nil {
		return invalidAddr, fmt.Errorf("trouble getting reg addr info for %s: %w", childNodePath, err)
	}

	physAddr, err := parseCells(numPAddrCells, &regByteContents)

	return physAddr, err
}

// Reads the /ranges file and converts the bytestream into integers representing the < child address parent address parent size >.
func getRangesAddr(childNodePath string, numCAddrCells, numPAddrCells, numAddrSpaceCells uint32) ([]rangeInfo, error) {
	childNodePath += "/ranges"
	childNodePath = cleanFilePath(childNodePath)
	var addrRanges []rangeInfo

	//nolint:gosec
	rangeByteContents, err := os.ReadFile(childNodePath)
	if err != nil {
		return addrRanges, fmt.Errorf("trouble getting reg addr info for %s: %w", childNodePath, err)
	}

	// read and decipher bytes for child address, parent address, and address space length from /ranges
	numRanges := uint32(len(rangeByteContents)) / (4 * (numCAddrCells + numPAddrCells + numAddrSpaceCells))

	for range numRanges {
		childAddr, err := parseCells(numCAddrCells, &rangeByteContents)
		if err != nil {
			return []rangeInfo{}, errors.New("error getting child address")
		}
		parentAddr, err := parseCells(numPAddrCells, &rangeByteContents)
		if err != nil {
			return []rangeInfo{}, errors.New("error getting parent address")
		}
		addrSpaceSize, err := parseCells(numAddrSpaceCells, &rangeByteContents)
		if err != nil {
			return []rangeInfo{}, errors.New("error getting address space size")
		}

		rangeInfo := rangeInfo{childAddr: childAddr, parentAddr: parentAddr, addrSpaceSize: addrSpaceSize}

		addrRanges = append(addrRanges, rangeInfo)
	}

	return addrRanges, err
}

// Recursively traverses device tree to calcuate physical address of specified GPIO chip.
func setGPIONodePhysAddrHelper(currNodePath, dtBaseNodePath string, physAddress uint64, numCAddrCells uint32) (uint64, error) {
	invalidAddr := uint64(math.NaN())

	// Base Case: We are at the root of the device tree.
	// ReplaceAll() accounts for the test case scenario, that uses the local device tree within the pi5 folder.
	// Having the "./" at the beginning of our file path messes with our path comparisons in the recursive calls, so we
	// remove it if its there just for the base node comparison step.
	if currNodePath == strings.ReplaceAll(dtBaseNodePath, "./", "") {
		return physAddress, nil
	}

	// Normal Case: We are not at the root of the device tree.
	// We must continue mapping our child addr (from the previous call) to this parent's addr space.
	parentNodePath := filepath.Dir(currNodePath)
	numPAddrCells, numAddrSpaceCells, err := getNumAddrSizeCells(parentNodePath)
	if err != nil {
		return invalidAddr, err
	}

	var addrRanges []rangeInfo
	// Case 1: We are the Child Node. No addr has been set. Read the reg file to get the physical address within our parents space.
	if physAddress == invalidAddr {
		physAddress, err = getRegAddr(currNodePath, numPAddrCells)
		if err != nil {
			return invalidAddr, err
		}
		// Case 2: We use the ranges property to continue mapping a child addr into our parent addr space.
	} else {
		addrRanges, err = getRangesAddr(currNodePath, numCAddrCells, numPAddrCells, numAddrSpaceCells)
		if err != nil {
			return invalidAddr, err
		}

		// getRangesAddr returns a list of all possible child address ranges our physical address can fall into.
		// We must see which range to use, so that we can map our physical address into the correct parent address range.
		for _, addrRange := range addrRanges {
			if addrRange.childAddr <= physAddress && physAddress <= addrRange.childAddr+addrRange.addrSpaceSize {
				physAddress -= addrRange.childAddr  // get the offset between the address and child base address
				physAddress += addrRange.parentAddr // now address has been mapped into parent space.
				break
			}
		}
	}

	numCAddrCells = numPAddrCells
	currNodePath = parentNodePath
	return setGPIONodePhysAddrHelper(currNodePath, dtBaseNodePath, physAddress, numCAddrCells)
}

// Uses information stored within the 'reg' property of the child node
// and 'ranges' property of its parents to map the child's physical address into the dev/gpiomem space.
func setGPIONodePhysAddr(nodePath, dtBaseNodePath string) (uint64, error) {
	var err error
	currNodePath := dtBaseNodePath + nodePath // example on pi5: /proc/device-tree/axi/pcie@120000/rp1/gpio@d0000
	invalidAddr := uint64(math.NaN())
	numCAddrCells := uint32(0)

	/* Call recursive function to calculate phys addr. Works way up the device tree, using the information
	found in #ranges at every node to translate from the child's address space to the parent's address space
	until we get the child's physical address in all of /dev/gpiomem. */
	physAddr, err := setGPIONodePhysAddrHelper(currNodePath, dtBaseNodePath, invalidAddr, numCAddrCells)
	if err != nil {
		return 0, fmt.Errorf("trouble calculating phys addr for %s: %w", nodePath, err)
	}

	return physAddr, nil
}

// Creates a virtual page to access/manipulate memory related to gpiochip data.
func createGPIOVPage(memPath string, chipSize uint64) (*os.File, mmap.MMap, *byte, error) {
	var err error

	/*
		Open the 'file' you are trying to map.
		Note: /dev/gpiomem0 is an device inode (not a file), so when .stat() is called on it to
		determine file length, it returns 0. However, you can still read starting from this address.
	*/

	fileFlags := os.O_RDWR | os.O_SYNC
	// 0666 is an octal representation of: file is readable / writeable by anyone
	//nolint:gosec
	memFile, err := os.OpenFile(memPath, fileFlags, 0o666)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open %s: %w", memPath, err)
	}

	/*
		In OS mmap() calls, virtual mapping to a physical page must start at the beginning of a physical
		page. This works great when those two values are aligned. However, when the starting address of data
		does not align with the beginning of a page, you must keep track of the difference between your data's
		starting address and its page's starting address. Your virtual space will need to extend that difference
		to properly map to the end of your desired space. This matters when the length of data you're trying to use
		spans one or more page boundaries.

		In this implementation, we know that our base address is aligned with the beginning of a page. However,
		below is the implementation required when they do not align.

		pageSize := uint64(syscall.Getpagesize())
		pageStart := b.physAddr & (pageSize - 1)
		// difference between base address of the page and the address we're actually tring to access
		dataStartingAddrDiff = b.physAddr - pageStart
		lenMapping := int(dataStartingAddrDiff)) + int(b.chipSize)
		b.vPage, err := mmap.MapRegion(b.memFile, lenMapping, mmap.RDWR, 0, 0)

		***** for the mmap() call ****
		- if we were using /dev/mem, then offset = pageStart.
		the file we 'open' starts at the base address of gpio memory for chip 0, not at the base of memory.
		- we would access our memory by accessing vPage[dataStartingAddrDiff] if the start address of the data != page start address

	*/

	// 0 flag = shared, 0 offset because we are starting from the beginning of the mem/gpiomem0 file. offs = pageStart if we opened /dev/mem
	vPage, err := mmap.MapRegion(memFile, int(chipSize), mmap.RDWR, 0, 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to mmap: %w", err)
	}

	virtAddr := &vPage[0]
	return memFile, vPage, virtAddr, nil
}

// SetupPinControl sets up GPIO pin memory access by parsing the device tree for relevant address information.
// Implementers will need to contsruct a PinctrlConfig to use this.
func SetupPinControl(cfg Config, logger logging.Logger) (Pinctrl, error) {
	var err error
	// This is not generalizeable; determine if there is a way to retrieve this from the pi / config / mapping information instead.

	// If we are running tests, we need to read files/folders from our module's local sample device tree.
	// This is located at raspi5-pinctrl/pi5/mock-device-tree
	testingMode := cfg.TestPath != ""
	dtBaseNodePath := cfg.getBaseNodePath()

	nodePath := cfg.GPIOChipPath
	logger.Info("no alias path: ", nodePath)
	if cfg.UseAlias {
		nodePath, err = findPathFromAlias(cfg.GPIOChipPath, dtBaseNodePath)
		if err != nil {
			logger.Errorf("error getting raspi5 GPIO nodePath")
			return Pinctrl{}, err
		}
		logger.Info("nodepath: ", nodePath)
	}

	physAddr, err := setGPIONodePhysAddr(nodePath, dtBaseNodePath)
	if err != nil {
		logger.Errorf("error getting raspi5 GPIO physical address")
		return Pinctrl{}, err
	}

	// In our current pinctrl_test.go we do not have any fake or real board
	// to run tests with. We exit since we cannot actually access memory,
	// which means we can't create a virtual page either.
	if testingMode {
		return Pinctrl{PhysAddr: physAddr, Cfg: cfg}, nil
	}

	memFile, vPage, virtAddr, err := createGPIOVPage(cfg.GPIOMemPath, cfg.ChipSize)
	if err != nil {
		logger.Errorf("error creating virtual page from GPIO physical address")
		return Pinctrl{}, err
	}

	return Pinctrl{MemFile: memFile, VPage: vPage, VirtAddr: virtAddr, PhysAddr: physAddr, Cfg: cfg, logger: logger}, nil
}

// Close cleans up mapped memory / files related to pin control upon board close() call.
func (ctrl *Pinctrl) Close() error {
	if ctrl.VPage != nil {
		if err := ctrl.VPage.Unmap(); err != nil {
			return fmt.Errorf("error during unmap: %w", err)
		}
	}
	if ctrl.MemFile != nil {
		if err := ctrl.MemFile.Close(); err != nil {
			return fmt.Errorf("error during memFile closing: %w", err)
		}
	}

	return nil
}
