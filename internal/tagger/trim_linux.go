//go:build tagger && linux

package tagger

// #include <malloc.h>
import "C"

// mallocTrim returns glibc's free heap tail to the kernel so memory ORT
// freed leaves RSS. On musl the symbol is a stub that returns 0.
func mallocTrim() {
	C.malloc_trim(0)
}
