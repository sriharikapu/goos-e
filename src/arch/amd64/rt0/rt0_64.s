; vim: set ft=nasm :
%include "constants.inc"

bits 64

section .bss
align 8

; According to the "ELF handling for TLS" document section 3.4.6
; (https://www.akkadia.org/drepper/tls.pdf) for the GNU variant for x86-64,
; fs:0x00 contains a pointer to the TCB. Variables in the TLS are stored
; before the TCB and are accessed using negative offsets from the TCB address.
r0_g_ptr:  resq 1
tcb_ptr:   resq 1

section .text

;------------------------------------------------------------------------------
; Kernel 64-bit entry point
;
; The 32-bit entrypoint code jumps to this entrypoint after:
; - it has entered long mode and enabled paging
; - it has loaded a 64bit GDT
; - it has set up identity paging for the physical 0-8M region and the
;   PAGE_OFFSET to PAGE_OFFSET+8M region.
;------------------------------------------------------------------------------
global _rt0_64_entry
_rt0_64_entry:
	call _rt0_install_redirect_trampolines
	call _rt0_64_setup_go_runtime_structs

	; Call the kernel entry point passing a pointer to the multiboot data
	; copied by the 32-bit entry code
	extern multiboot_data
	extern _kernel_start
	extern _kernel_end
	extern kernel.Kmain

	mov rax, PAGE_OFFSET
	push rax
	mov rax, _kernel_end - PAGE_OFFSET
	push rax
	mov rax, _kernel_start - PAGE_OFFSET
	push rax
	mov rax, multiboot_data
	push rax
	call kernel.Kmain

	; Main should never return; halt the CPU
	mov rdi, err_kmain_returned
	call write_string

	cli
	hlt

;------------------------------------------------------------------------------
; Setup m0, g0 and other symbols required for bootstrapping the Go runtime.
; For the definitions of g and m see the Go runtime src: src/runtime/runtime2.go
;------------------------------------------------------------------------------
_rt0_64_setup_go_runtime_structs:
	%include "go_asm_offsets.inc" ; generated by tools/offsets

	%ifndef SKIP_PAGESIZE_SETUP
	; The Go allocator expects this symbol to be set to the system page size
	; As the kernel bypasses osinit() this needs to be manually set here.
	extern runtime.physPageSize
	mov rax, runtime.physPageSize
	mov qword [rax], 0x1000 ; 4096
	%endif

	; Setup r0_g stack limits using the reserved stack
	extern stack_top
	extern stack_bottom
	extern runtime.g0
	mov rax, stack_bottom
	mov rbx, stack_top
	mov rsi, runtime.g0
	mov qword [rsi+GO_G_STACK+GO_STACK_LO], rax   ; g.stack.lo
	mov qword [rsi+GO_G_STACK+GO_STACK_HI], rbx   ; g.stack.hi
	mov qword [rsi+GO_G_STACKGUARD0], rax         ; g.stackguard0

	; Link m0 to the g0
	extern runtime.m0
	mov rbx, runtime.m0
	mov qword [rbx+GO_M_CURG], rsi     ; m.curg = g0
	mov qword [rbx+GO_M_G0], rsi       ; m.g0 = g0
	mov qword [rsi+GO_G_M], rbx        ; g.m = m

	; Store the address of g0 in r0_g_ptr
	mov rax, r0_g_ptr
	mov qword [rax], rsi

	; According to the x86-64 ABI requirements fs:0x0 should point to the
	; TCB.
	mov rax, tcb_ptr
	mov qword [rax], rax

	; Load 64-bit FS register address
	; eax -> lower 32 bits
	; edx -> upper 32 bits
	mov ecx, 0xc0000100  ; fs_base
	mov rsi, tcb_ptr
	mov rax, rsi         ; lower 32 bits
	shr rsi, 32
	mov rdx, rsi         ; high 32 bits
	wrmsr

	ret

;------------------------------------------------------------------------------
; Error messages
;------------------------------------------------------------------------------
err_kmain_returned db '[rt0_64] kmain returned', 0

;------------------------------------------------------------------------------
; Write the NULL-terminated string contained in rdi to the screen using white
; text on red background.  Assumes that text-mode is enabled and that its
; physical address is 0xb8000.
;------------------------------------------------------------------------------
write_string:
	mov rbx,0xb8000
	mov ah, 0x4F
.next_char:
	mov al, byte[rdi]
	test al, al
	jz write_string.done

	mov word [rbx], ax
	add rbx, 2
	inc rdi
	jmp write_string.next_char

.done:
	ret

;------------------------------------------------------------------------------
; Install redirect trampolines. This hack allows us to redirect calls to Go
; runtime functions to the kernel's own implementation without the need to
; export/globalize any symbols. This works by first setting up a redirect table
; (populated by a post-link step) that contains the addresses of the symbol to
; hook and the address where calls to that symbol should be redirected.
;
; This function iterates the redirect table entries and for each entry it
; sets up a trampoline to the dst symbol and overwrites the code in src with
; the 14-byte long _rt0_redirect_trampoline code.
;
; Note: this code modification is only possible because we are currently
; operating in supervisor mode with no memory protection enabled. Under normal
; conditions the .text section should be flagged as read-only.
;------------------------------------------------------------------------------
_rt0_install_redirect_trampolines:
	mov rax, _rt0_redirect_table
	mov rdx, NUM_REDIRECTS

_rt0_install_redirect_rampolines.next:
	mov rdi, [rax]	 ; the symbol address to hook
	mov rbx, [rax+8] ; the symbol to redirect to

	; setup trampoline target and copy it to the hooked symbol
	mov rsi, _rt0_redirect_trampoline
	mov qword [rsi+6], rbx
	mov rcx, 14
	rep movsb ; copy rcx bytes from rsi to rdi

	add rax, 16
	dec rdx
	jnz _rt0_install_redirect_rampolines.next

	ret

;------------------------------------------------------------------------------
; This trampoline exploits rip-relative addressing to allow a jump to a
; 64-bit address without the need to touch any registers. The generated
; code is equivalent to:
;
; jmp [rip+0]
; dq abs_address_to_jump_to
;------------------------------------------------------------------------------
_rt0_redirect_trampoline:
	db 0xff ; the first 6 bytes encode a "jmp [rip+0]" instruction
	db 0x25
	dd 0x00
	dq 0x00 ; the absolute address to jump to

;------------------------------------------------------------------------------
; The redirect table is placed in a dedicated section allowing us to easily
; find its offset in the kernel image file. As the VMA addresses of the src
; and target symbols for the redirect are now known in advance we just reserve
; enough space space for the src and dst addresses using the NUM_REDIRECTS
; define which is calculated by the Makefile and passed to nasm.
;------------------------------------------------------------------------------
section .goredirectstbl

_rt0_redirect_table:
	%rep NUM_REDIRECTS
	dq 0  ; src: address of the symbol we want to redirect
	dq 0  ; dst: address of the symbol where calls to src are redirected to
	%endrep
