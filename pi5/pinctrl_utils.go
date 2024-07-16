//go:build linux

package pi5

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	mmap "github.com/edsrzf/mmap-go"
	"github.com/pkg/errors"
)

// rangeInfo represents the info provided in the ranges property of a device tree. It provides a mapping between // registers in the child address space to the parent address space.
type rangeInfo struct {
	childAddr     uint64
	parentAddr    uint64
	addrSpaceSize uint64
}

var INVALID_ADDR uint64 = uint64(math.NaN())

const gpioName = "gpio0"
const gpioMemPath = "/dev/gpiomem0"
const dtBaseNodePath = "/proc/device-tree"

// Sets up GPIO Pin Memory Access by parsing the device tree for relevant address information
func (b *pinctrlpi5) setupPinControl() error {
	nodePath, err := b.findPathFromAlias(gpioName) // this ("gpio") is hardcoded now, we will fix that later!
	if err != nil {
		b.logger.Errorf("error getting raspi5 GPIO nodePath")
		return err
	}

	err = b.setGPIONodePhysAddr(nodePath)
	if err != nil {
		b.logger.Errorf("error getting raspi5 GPIO physical address")
		return err
	}

	err = b.createGPIOVPage(gpioMemPath)
	if err != nil {
		b.logger.Errorf("error creating virtual page from GPIO physical address")
		return err
	}

	return err
}

// We look in the 'aliases' node at the base of proc/device-tree to determine the full file path required to access our GPIO Chip
func (b *pinctrlpi5) findPathFromAlias(nodeName string) (string, error) {

	dtNodePath := dtBaseNodePath + "/aliases/" + nodeName
	nodePathBytes, err := os.ReadFile(dtNodePath)
	if err != nil {
		return "", fmt.Errorf("Error reading directory: %w", err)
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

// removes nonprintable characters + other random characters from file path before opening files in device tree
func cleanFilePath(childNodePath string) string {
	childNodePath = strings.TrimSpace(childNodePath)
	re := regexp.MustCompile(`[\x00-\x1F\x7F-\x9F]`) // gets rid of random non printable chars. works for now but make cleaner later
	childNodePath = re.ReplaceAllString(childNodePath, "")
	return childNodePath
}

// Reads the /reg file and converts the bytestream into a uint64 representing the GPIO Node's physical address within its parent's space. (pre address mapping)
func getRegAddr(childNodePath string, numPAddrCells uint32) (uint64, error) {

	childNodePath += "/reg"
	childNodePath = cleanFilePath(childNodePath)

	regByteContents, err := os.ReadFile(childNodePath)
	if err != nil {
		return INVALID_ADDR, fmt.Errorf("trouble getting reg addr info for %s: %w\n", childNodePath, err)
	}

	physAddr, err := parseCells(numPAddrCells, &regByteContents)

	return physAddr, err
}

// read in 'numCells' 32 bit chunks from byteContents, the bytestream outputted from reading the file '/ranges'. Convert bytes into their uint64 value
func parseCells(numCells uint32, byteContents *[]byte) (uint64, error) {
	var parsedValue uint64

	if len(*byteContents) < int(numCells)*4 {
		errorMsg := "num cells was: " + string(numCells) + ", but there aren't enough bytes to read in from inputted bytestream"
		return 0, errors.New(errorMsg)
	}

	switch numCells {

	// reading in 32 bits. regardless we must convert to a 64 bit address so we add a bunch of 0s to the beginning.
	case 1:
		parsedValue = uint64(binary.BigEndian.Uint32((*byteContents)[:(4 * numCells)]))

	// reading in 64 bits
	case 2:
		parsedValue = binary.BigEndian.Uint64((*byteContents)[:(4 * numCells)])

	// reading in more than 64 bits. we only want the last 64 bits of the address so we cut off the other portion
	default:
		parsedValue = binary.BigEndian.Uint64((*byteContents)[(4 * (numCells - 2)):(4 * numCells)])
	}

	*byteContents = (*byteContents)[(4 * numCells):] // flush the bytes already parsed out of the array
	return parsedValue, nil
}

// Reads the /ranges file and converts the bytestream into integers representing the < child address parent address parent size >
func getRangesAddrInfo(childNodePath string, numCAddrCells uint32, numPAddrCells uint32, numAddrSpaceCells uint32) ([]rangeInfo, error) {

	childNodePath += "/ranges"
	childNodePath = cleanFilePath(childNodePath)
	var addrRanges []rangeInfo

	rangeByteContents, err := os.ReadFile(childNodePath)
	if err != nil {
		return addrRanges, fmt.Errorf("trouble getting reg addr info for %s: %w\n", childNodePath, err)
	}

	// read and decipher bytes for child address, parent address, and address space length from /ranges
	numRanges := uint32(len(rangeByteContents)) / (4 * (numCAddrCells + numPAddrCells + numAddrSpaceCells))

	for i := uint32(0); i < numRanges; i++ {

		childAddr, parentAddr := uint64(math.NaN()), uint64(math.NaN())
		addrSpaceSize := uint64(0)

		childAddr, err = parseCells(numCAddrCells, &rangeByteContents)
		parentAddr, err = parseCells(numPAddrCells, &rangeByteContents)
		addrSpaceSize, err = parseCells(numAddrSpaceCells, &rangeByteContents)

		rangeInfo := rangeInfo{childAddr: childAddr, parentAddr: parentAddr, addrSpaceSize: addrSpaceSize}
		addrRanges = append(addrRanges, rangeInfo)
	}

	return addrRanges, err
}

// Uses Information Stored within the 'reg' property of the child node and 'ranges' property of its parents to map the child's physical address into the dev/gpiomem space
func (b *pinctrlpi5) setGPIONodePhysAddr(nodePath string) error {

	currNodePath := dtBaseNodePath + nodePath // initially: /proc/device-tree/axi/pcie@120000/rp1/gpio@d0000
	var numCAddrCells uint32 = 0
	var err error

	/* Call Recursive Function to Calculate Phys Addr. Works way up the Device Tree, using the information
	found in #ranges at every node to translate from the child's address space to the parent's address space
	until we get the child's physical address in all of /dev/gpiomem. */
	b.physAddr, err = setGPIONodePhysAddrHelper(currNodePath, INVALID_ADDR, numCAddrCells)
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

	var addrRanges []rangeInfo
	if physAddress == INVALID_ADDR { // Case 1: We are the Child Node. No addr has been set. Read the reg file to get the physical address within our parents space.
		physAddress, err = getRegAddr(currNodePath, numPAddrCells)
		if err != nil {
			return INVALID_ADDR, err
		}

	} else { // Case 2: We use the ranges property to continue mapping a child addr into our parent addr space.
		addrRanges, err = getRangesAddrInfo(currNodePath, numCAddrCells, numPAddrCells, numAddrSpaceCells)
		if err != nil {
			return INVALID_ADDR, err
		}

		// getRangesAddrInfo returns a list of all possible child address ranges our physical address can fall into. We must see which range to use, so that we can map our physical address into the correct parent address range.
		for _, addrRange := range addrRanges {

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

// Creates a virtual Page to access/manipulate memory related to gpiochip data
func (b *pinctrlpi5) createGPIOVPage(memPath string) error {
	var err error

	/*
		Open the 'file' you are trying to map.
		Note: /dev/gpiomem0 is an device inode (not a file), so when .stat() is called on it to
		determine file length, it returns 0. However, you can still read starting from this address.
	*/

	fileFlags := os.O_RDWR | os.O_SYNC
	b.memFile, err = os.OpenFile("/dev/gpiomem0", fileFlags, 0666) // 0666 is an octal representation of: file is readable / writeable by anyone
	if err != nil {
		return fmt.Errorf("failed to open %s: %w\n", memPath, err)
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
		dataStartingAddrDiff  b.physAddr - pageStart 							// difference between base address of the page and the address we're actually tring to access
		lenMapping := int(dataStartingAddrDiff)) + int(b.chipSize)
		b.vPage, err := mmap.MapRegion(b.memFile, lenMapping, mmap.RDWR, 0, 0)

		***** for the mmap() call ****
		- if we were using dev/mem, then offset = pageStart. the file we 'open' starts at the base address of gpio memory for chip 0, not at the base of memory.
		- we would access our memory by accessing vPage[dataStartingAddrDiff] if the start address of the data != page start address

	*/

	b.vPage, err = mmap.MapRegion(b.memFile, int(b.chipSize), mmap.RDWR, 0, 0) // 0 flag = shared, 0 offset because we are starting from the beginning of the mem/gpiomem0 file. offs = pageStart if we opened dev/mem
	if err != nil {
		return fmt.Errorf("failed to mmap: %w\n", err)
	}

	b.virtAddr = &b.vPage[0]
	return err
}

// Cleans up mapped memory / files upon board close() call
func (b *pinctrlpi5) cleanupPinControlMemory() error {

	if err := b.vPage.Unmap(); err != nil {
		return fmt.Errorf("Error during unmap: %w", err)
	}

	if err := b.memFile.Close(); err != nil {
		return fmt.Errorf("Error during memFile closing: %w", err)
	}

	return nil
}
