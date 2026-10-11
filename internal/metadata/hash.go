package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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

	var stepsStr, cfgStr string
	if steps != nil {
		stepsStr = strconv.Itoa(*steps)
	}
	if cfg != nil {
		// Two decimals absorb float noise.
		cfgStr = fmt.Sprintf("%.2f", *cfg)
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "prompt=%s\nnegative=%s\nmodel=%s\nsampler=%s\nsteps=%s\ncfg=%s\n",
		prompt, negPrompt, model, sampler, stepsStr, cfgStr))
	return hex.EncodeToString(sum[:])[:genHashLen]
}
