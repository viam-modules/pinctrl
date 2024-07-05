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
)

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

const dtBaseNodePath = "/proc/device-tree"

// Sets up GPIO Pin Memory Access by parsing the device tree for relevant address information
func (b *pinctrlpi5) pinControlSetup() error {
	nodePath, err := b.findPathFromAlias("gpio0") // this ("gpio") is hardcoded now, we will fix that later!
	if err != nil {
		b.logger.Errorf("error getting raspi5 GPIO nodePath")
	}

	err = b.setGPIONodePhysAddr(nodePath)
	if err != nil {
		b.logger.Errorf("error getting raspi5 GPIO physical address")
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
