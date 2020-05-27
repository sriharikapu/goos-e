package pmm

import (
	"goose/kernel"
	"goose/kernel/kfmt"
	"goose/kernel/mm"
	"goose/multiboot"
)

var (
	errBootAllocOutOfMemory = &kernel.Error{Module: "boot_mem_alloc", Message: "out of memory"}
)

// BootMemAllocator implements a rudimentary physical memory allocator which is
// used to bootstrap the kernel.
//
// The allocator implementation uses the memory region information provided by
// the bootloader to detect free memory blocks and return the next available
// free frame.  Allocations are tracked via an internal counter that contains
// the last allocated frame.
//
// Due to the way that the allocator works, it is not possible to free
// allocated pages. Once the kernel is properly initialized, the allocated
// blocks will be handed over to a more advanced memory allocator that does
// support freeing.
type BootMemAllocator struct {
	// allocCount tracks the total number of allocated frames.
	allocCount uint64

	// lastAllocFrame tracks the last allocated frame number.
	lastAllocFrame mm.Frame

	// Keep track of kernel location so we exclude this region.
	kernelStartAddr, kernelEndAddr   uintptr
	kernelStartFrame, kernelEndFrame mm.Frame
}

// init sets up the boot memory allocator internal state.
func (alloc *BootMemAllocator) init(kernelStart, kernelEnd uintptr) {
	// round down kernel start to the nearest page and round up kernel end
	// to the nearest page.
	pageSizeMinus1 := mm.PageSize - 1
	alloc.kernelStartAddr = kernelStart
	alloc.kernelEndAddr = kernelEnd
	alloc.kernelStartFrame = mm.Frame((kernelStart & ^pageSizeMinus1) >> mm.PageShift)
	alloc.kernelEndFrame = mm.Frame(((kernelEnd+pageSizeMinus1) & ^pageSizeMinus1)>>mm.PageShift) - 1

}

// AllocFrame scans the system memory regions reported by the bootloader and
// reserves the next available free frame.
//
// AllocFrame returns an error if no more memory can be allocated.
func (alloc *BootMemAllocator) AllocFrame() (mm.Frame, *kernel.Error) {
	var err = errBootAllocOutOfMemory

	multiboot.VisitMemRegions(func(region *multiboot.MemoryMapEntry) bool {
		// Ignore reserved regions and regions smaller than a single page
		if region.Type != multiboot.MemAvailable || region.Length < uint64(mm.PageSize) {
			return true
		}

		// Reported addresses may not be page-aligned; round up to get
		// the start frame and round down to get the end frame
		pageSizeMinus1 := uint64(mm.PageSize - 1)
		regionStartFrame := mm.Frame(((region.PhysAddress + pageSizeMinus1) & ^pageSizeMinus1) >> mm.PageShift)
		regionEndFrame := mm.Frame(((region.PhysAddress+region.Length) & ^pageSizeMinus1)>>mm.PageShift) - 1

		// Skip over already allocated regions
		if alloc.lastAllocFrame >= regionEndFrame {
			return true
		}

		// If last frame used a different region and the kernel image
		// is located at the beginning of this region OR we are in
		// current region but lastAllocFrame + 1 points to the kernel
		// start we need to jump to the page following the kernel end
		// frame
		if (alloc.lastAllocFrame <= regionStartFrame && alloc.kernelStartFrame == regionStartFrame) ||
			(alloc.lastAllocFrame <= regionEndFrame && alloc.lastAllocFrame+1 == alloc.kernelStartFrame) {
			//fmt.Printf("last: %d, case: 1, set last: %d\n", alloc.lastAllocFrame, alloc.kernelEndFrame+1)
			alloc.lastAllocFrame = alloc.kernelEndFrame + 1
		} else if alloc.lastAllocFrame < regionStartFrame || alloc.allocCount == 0 {
			// we are in the previous region and need to jump to this one OR
			// this is the first allocation and the region begins at frame 0
			//fmt.Printf("last: %d, case: 2, set last: %d\n", alloc.lastAllocFrame, regionStartFrame)
			alloc.lastAllocFrame = regionStartFrame
		} else {
			// we are in the region and we can select the next frame
			//fmt.Printf("last: %d, case: 3, set last: %d\n", alloc.lastAllocFrame, alloc.lastAllocFrame+1)
			alloc.lastAllocFrame++
		}

		// The above adjustment might push lastAllocFrame outside of the
		// region end (e.g kernel ends at last page in the region)
		if alloc.lastAllocFrame > regionEndFrame {
			return true
		}

		err = nil
		return false
	})

	if err != nil {
		return mm.InvalidFrame, errBootAllocOutOfMemory
	}

	alloc.allocCount++
	return alloc.lastAllocFrame, nil
}

// printMemoryMap scans the memory region information provided by the
// bootloader and prints out the system's memory map.
func (alloc *BootMemAllocator) printMemoryMap() {
	kfmt.Printf("[boot_mem_alloc] system memory map:\n")
	var totalFree uint64
	multiboot.VisitMemRegions(func(region *multiboot.MemoryMapEntry) bool {
		kfmt.Printf("\t[0x%10x - 0x%10x], size: %10d, type: %s\n", region.PhysAddress, region.PhysAddress+region.Length, region.Length, region.Type.String())

		if region.Type == multiboot.MemAvailable {
			totalFree += region.Length
		}
		return true
	})
	kfmt.Printf("[boot_mem_alloc] available memory: %dKb\n", totalFree/1024)
	kfmt.Printf("[boot_mem_alloc] kernel loaded at 0x%x - 0x%x\n", alloc.kernelStartAddr, alloc.kernelEndAddr)
	kfmt.Printf("[boot_mem_alloc] size: %d bytes, reserved pages: %d\n",
		uint64(alloc.kernelEndAddr-alloc.kernelStartAddr),
		uint64(alloc.kernelEndFrame-alloc.kernelStartFrame+1),
	)
}
