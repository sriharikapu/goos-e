package kernel

// Error describes a kernel error. All kernel errors must be defined as global
// variables that are pointers to the Error structure. This requirement stems
// from the fact that the Go allocator is not available to us so we cannot use
// errors.New.
type Error struct {
	// The module where the error occurred.
	Module string

	// The error message
	Message string
}

// Error implements the error interface.
// when error occurs
func (e *Error) Error() string {
	return e.Message
}
