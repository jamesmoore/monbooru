package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// 48 bits: short enough to paste into a search bar, enough for a personal
// library.
const genHashLen = 12

// Seed is left out so re-rolls of one recipe share a hash; empty inputs
// give "" so images without metadata do not all share one.
func computeGenerationHash(prompt, negPrompt, model, sampler string, steps *int, cfg *float64) string {
	prompt = strings.TrimSpace(prompt)
	negPrompt = strings.TrimSpace(negPrompt)
	model = strings.TrimSpace(model)
	sampler = strings.TrimSpace(sampler)

	if prompt == "" && negPrompt == "" && model == "" && sampler == "" && steps == nil && cfg == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("prompt=")
	b.WriteString(prompt)
	b.WriteByte('\n')
	b.WriteString("negative=")
	b.WriteString(negPrompt)
	b.WriteByte('\n')
	b.WriteString("model=")
	b.WriteString(model)
	b.WriteByte('\n')
	b.WriteString("sampler=")
	b.WriteString(sampler)
	b.WriteByte('\n')
	b.WriteString("steps=")
	if steps != nil {
		fmt.Fprintf(&b, "%d", *steps)
	}
	b.WriteByte('\n')
	b.WriteString("cfg=")
	if cfg != nil {
		// Two decimals absorb float noise.
		fmt.Fprintf(&b, "%.2f", *cfg)
	}
	b.WriteByte('\n')

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:genHashLen]
}
