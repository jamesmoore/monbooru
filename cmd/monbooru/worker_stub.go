//go:build !tagger

package main

import (
	"fmt"
	"os"
)

func runWorker(_ []string) {
	fmt.Fprintln(os.Stderr, "tagger-worker: this binary was built without -tags tagger")
	os.Exit(2)
}
