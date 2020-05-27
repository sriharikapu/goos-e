package pmm

import (
	"goose/kernel"
	"goose/kernel/kfmt"
	"goose/kernel/mm"
	"goose/kernel/mm/vmm"
	"goose/kernel/sync"
	"goose/multiboot"
	"math"
	"reflect"
	"unsafe"
)

var (
	errBitmapAllocOutOfMemory     = &kernel.Error{Module: "bitmap_alloc", Message: "out of memory"}
	errBitmapAllocFrameNotManaged = &kernel.Error{Module: "bitmap_alloc", Message: "frame not managed by this allocator"}
	errBitmapAllocDoubleFree      = &kernel.Error{Module: "bitmap_alloc", Message: "frame is already free"}

	// The followning functions are used by tests to mock calls to the vmm package
	// and are automatically inlined by the compiler.
	reserveRegionFn = vmm.EarlyReserveRegion
	mapFn           = vmm.Map
)

type markAs bool

const (
	markReserved markAs = false
	markFree            = true
)

type framePool struct {
	// startFrame is the frame number for the first page in this pool.
	// each free bitmap entry i corresponds to frame (startFrame + i).
	startFrame mm.Frame

	// endFrame tracks the last frame in the pool. The total number of
	// frames is given by: (endFrame - startFrame) - 1
	endFrame mm.Frame

	// freeCount tracks the available pages in this pool. The allocator
	// can use this field to skip fully allocated pools without the need
	// to scan the free bitmap.
	freeCount uint32

	// freeBitmap tracks used/free pages in the pool.
	freeBitmap    []uint64
	freeBitmapHdr reflect.SliceHeader
}

// BitmapAllocator implements a physical frame allocator that tracks frame
// reservations across the available memory pools using bitmaps.
type BitmapAllocator struct {
	mutex sync.Spinlock

	// totalPages tracks the total number of pages across all pools.
	totalPages uint32

	// reservedPages tracks the number of reserved pages across all pools.
	reservedPages uint32

	pools    []framePool
	poolsHdr reflect.SliceHeader
}

// init allocates space for the allocator structures using the early bootmem
// allocator and flags any allocated pages as reserved.
func (alloc *BitmapAllocator) init() *kernel.Error {
	if err := alloc.setupPoolBitmaps(); err != nil {
		return err
	}

	alloc.reserveKernelFrames()
	alloc.reserveEarlyAllocatorFrames()
	alloc.printStats()
	return nil
}

// setupPoolBitmaps uses the early allocator and vmm region reservation helper
// to initialize the list of available pools and their free bitmap slices.
func (alloc *BitmapAllocator) setupPoolBitmaps() *kernel.Error {
	var (
		err                 *kernel.Error
		sizeofPool          = unsafe.Sizeof(framePool{})
		pageSizeMinus1      = mm.PageSize - 1
		requiredBitmapBytes uint64
	)

	// Detect available memory regions and calculate their pool bitmap
	// requirements.
	multiboot.VisitMemRegions(func(region *multiboot.MemoryMapEntry) bool {
		if region.Type != multiboot.MemAvailable {
			return true
		}

		alloc.poolsHdr.Len++
		alloc.poolsHdr.Cap++

		// Reported addresses may not be page-aligned; round up to get
		// the start frame and round down to get the end frame
		regionStartFrame := mm.Frame(((uintptr(region.PhysAddress) + pageSizeMinus1) & ^pageSizeMinus1) >> mm.PageShift)
		regionEndFrame := mm.Frame((uintptr(region.PhysAddress+region.Length) & ^pageSizeMinus1)>>mm.PageShift) - 1
		pageCount := uint32(regionEndFrame - regionStartFrame)
		alloc.totalPages += pageCount

		// To represent the free page bitmap we need pageCount bits. Since our
		// slice uses uint64 for storing the bitmap we need to round up the
		// required bits so they are a multiple of 64 bits
		requiredBitmapBytes += uint64(((pageCount + 63) &^ 63) >> 3)
		return true
	})

	// Reserve enough pages to hold the allocator state
	requiredBytes := (uintptr(alloc.poolsHdr.Len)*sizeofPool + uintptr(requiredBitmapBytes) + pageSizeMinus1) & ^pageSizeMinus1
	requiredPages := requiredBytes >> mm.PageShift
	alloc.poolsHdr.Data, err = reserveRegionFn(requiredBytes)
	if err != nil {
		return err
	}

	for page, index := mm.PageFromAddress(alloc.poolsHdr.Data), uintptr(0); index < requiredPages; page, index = page+1, index+1 {
		nextFrame, err := earlyAllocFrame()
		if err != nil {
			return err
		}

		if err = mapFn(page, nextFrame, vmm.FlagPresent|vmm.FlagRW|vmm.FlagNoExecute); err != nil {
			return err
		}

		kernel.Memset(page.Address(), 0, mm.PageSize)
	}

	alloc.pools = *(*[]framePool)(unsafe.Pointer(&alloc.poolsHdr))

	// Run a second pass to initialize the free bitmap slices for all pools
	bitmapStartAddr := alloc.poolsHdr.Data + uintptr(alloc.poolsHdr.Len)*sizeofPool
	poolIndex := 0
	multiboot.VisitMemRegions(func(region *multiboot.MemoryMapEntry) bool {
		if region.Type != multiboot.MemAvailable {
			return true
		}

		regionStartFrame := mm.Frame(((uintptr(region.PhysAddress) + pageSizeMinus1) & ^pageSizeMinus1) >> mm.PageShift)
		regionEndFrame := mm.Frame((uintptr(region.PhysAddress+region.Length) & ^pageSizeMinus1)>>mm.PageShift) - 1
		bitmapBytes := ((uintptr(regionEndFrame-regionStartFrame) + 63) &^ 63) >> 3

		alloc.pools[poolIndex].startFrame = regionStartFrame
		alloc.pools[poolIndex].endFrame = regionEndFrame
		alloc.pools[poolIndex].freeCount = uint32(regionEndFrame - regionStartFrame + 1)
		alloc.pools[poolIndex].freeBitmapHdr.Len = int(bitmapBytes >> 3)
		alloc.pools[poolIndex].freeBitmapHdr.Cap = alloc.pools[poolIndex].freeBitmapHdr.Len
		alloc.pools[poolIndex].freeBitmapHdr.Data = bitmapStartAddr
		alloc.pools[poolIndex].freeBitmap = *(*[]uint64)(unsafe.Pointer(&alloc.pools[poolIndex].freeBitmapHdr))

		bitmapStartAddr += bitmapBytes
		poolIndex++
		return true
	})

	return nil
}

// markFrame updates the reservation flag for the bitmap entry that corresponds
// to the supplied frame.
func (alloc *BitmapAllocator) markFrame(poolIndex int, frame mm.Frame, flag markAs) {
	if poolIndex < 0 || frame > alloc.pools[poolIndex].endFrame {
		return
	}

	// The offset in the block is given by: frame % 64. As the bitmap uses a
	// big-ending representation we need to set the bit at index: 63 - offset
	relFrame := frame - alloc.pools[poolIndex].startFrame
	block := relFrame >> 6
	mask := uint64(1 << (63 - (relFrame - block<<6)))
	switch flag {
	case markFree:
		alloc.pools[poolIndex].freeBitmap[block] &^= mask
		alloc.pools[poolIndex].freeCount++
		alloc.reservedPages--
	case markReserved:
		alloc.pools[poolIndex].freeBitmap[block] |= mask
		alloc.pools[poolIndex].freeCount--
		alloc.reservedPages++
	}
}

// poolForFrame returns the index of the pool that contains frame or -1 if
// the frame is not contained in any of the available memory pools (e.g it
// points to a reserved memory region).
func (alloc *BitmapAllocator) poolForFrame(frame mm.Frame) int {
	for poolIndex, pool := range alloc.pools {
		if frame >= pool.startFrame && frame <= pool.endFrame {
			return poolIndex
		}
	}

	return -1
}

// reserveKernelFrames makes as reserved the bitmap entries for the frames
// occupied by the kernel image.
func (alloc *BitmapAllocator) reserveKernelFrames() {
	// Flag frames used by kernel image as reserved. Since the kernel must
	// occupy a contiguous memory block we assume that all its frames will
	// fall into one of the available memory pools
	poolIndex := alloc.poolForFrame(bootMemAllocator.kernelStartFrame)
	for frame := bootMemAllocator.kernelStartFrame; frame <= bootMemAllocator.kernelEndFrame; frame++ {
		alloc.markFrame(poolIndex, frame, markReserved)
	}
}

// reserveEarlyAllocatorFrames makes as reserved the bitmap entries for the frames
// already allocated by the early allocator.
func (alloc *BitmapAllocator) reserveEarlyAllocatorFrames() {
	// We now need to decomission the early allocator by flagging all frames
	// allocated by it as reserved. The allocator itself does not track
	// individual frames but only a counter of allocated frames. To get
	// the list of frames we reset its internal state and "replay" the
	// allocation requests to get the correct frames.
	allocCount := bootMemAllocator.allocCount
	bootMemAllocator.allocCount, bootMemAllocator.lastAllocFrame = 0, 0
	for i := uint64(0); i < allocCount; i++ {
		frame, _ := bootMemAllocator.AllocFrame()
		alloc.markFrame(
			alloc.poolForFrame(frame),
			frame,
			markReserved,
		)
	}
}

func (alloc *BitmapAllocator) printStats() {
	kfmt.Printf(
		"[bitmap_alloc] page stats: free: %d/%d (%d reserved)\n",
		alloc.totalPages-alloc.reservedPages,
		alloc.totalPages,
		alloc.reservedPages,
	)
}

// AllocFrame reserves and returns a physical memory frame. An error will be
// returned if no more memory can be allocated.
func (alloc *BitmapAllocator) AllocFrame() (mm.Frame, *kernel.Error) {
	alloc.mutex.Acquire()

	for poolIndex := 0; poolIndex < len(alloc.pools); poolIndex++ {
		if alloc.pools[poolIndex].freeCount == 0 {
			continue
		}

		fullBlock := uint64(math.MaxUint64)
		for blockIndex, block := range alloc.pools[poolIndex].freeBitmap {
			if block == fullBlock {
				continue
			}

			// Block has at least one free slot; we need to scan its bits
			for blockOffset, mask := 0, uint64(1<<63); mask > 0; blockOffset, mask = blockOffset+1, mask>>1 {
				if block&mask != 0 {
					continue
				}

				alloc.pools[poolIndex].freeCount--
				alloc.pools[poolIndex].freeBitmap[blockIndex] |= mask
				alloc.reservedPages++
				alloc.mutex.Release()
				return alloc.pools[poolIndex].startFrame + mm.Frame((blockIndex<<6)+blockOffset), nil
			}
		}
	}

	alloc.mutex.Release()
	return mm.InvalidFrame, errBitmapAllocOutOfMemory
}

// FreeFrame releases a frame previously allocated via a call to AllocFrame.
// Trying to release a frame not part of the allocator pools or a frame that
// is already marked as free will cause an error to be returned.
func (alloc *BitmapAllocator) FreeFrame(frame mm.Frame) *kernel.Error {
	alloc.mutex.Acquire()

	poolIndex := alloc.poolForFrame(frame)
	if poolIndex < 0 {
		alloc.mutex.Release()
		return errBitmapAllocFrameNotManaged
	}

	relFrame := frame - alloc.pools[poolIndex].startFrame
	block := relFrame >> 6
	mask := uint64(1 << (63 - (relFrame - block<<6)))

	if alloc.pools[poolIndex].freeBitmap[block]&mask == 0 {
		alloc.mutex.Release()
		return errBitmapAllocDoubleFree
	}

	alloc.pools[poolIndex].freeBitmap[block] &^= mask
	alloc.pools[poolIndex].freeCount++
	alloc.reservedPages--
	alloc.mutex.Release()
	return nil
}
