package metadata

import (
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
)

func extractSDFromJPEG(path string) (*models.SDMetadata, error) {
	x := decodeJPEGEXIF(path)
	if x == nil {
		return nil, nil
	}
	return sdFromEXIF(x), nil
}

func sdFromEXIF(x *exifData) *models.SDMetadata {
	tag, ok := x.get(userCommentField)
	if !ok {
		return nil
	}
	text, ok := tag.userCommentText()
	if !ok {
		return nil
	}
	return parseA1111Parameters(text)
}

// The "Negative prompt:" and "Steps:" markers tell A1111 text from what
// other tools write into UserComment.
func parseA1111Parameters(text string) *models.SDMetadata {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	negIdx := strings.Index(text, "\nNegative prompt:")
	paramIdx := findParamLineIndex(text)
	if negIdx < 0 && paramIdx < 0 {
		return nil
	}

	sd := &models.SDMetadata{}
	var rawParams string
	if negIdx >= 0 {
		sd.Prompt = strings.TrimSpace(text[:negIdx])
		afterNeg := text[negIdx+len("\nNegative prompt:"):]
		if paramIdx > negIdx {
			relIdx := paramIdx - negIdx - len("\nNegative prompt:")
			if relIdx > 0 && relIdx <= len(afterNeg) {
				sd.NegativePrompt = strings.TrimSpace(afterNeg[:relIdx])
			}
			rawParams = strings.TrimSpace(text[paramIdx:])
			parseA1111Params(rawParams, sd)
		} else {
			sd.NegativePrompt = strings.TrimSpace(afterNeg)
		}
	} else {
		sd.Prompt = strings.TrimSpace(text[:paramIdx])
		rawParams = strings.TrimSpace(text[paramIdx:])
		parseA1111Params(rawParams, sd)
	}

	sd.RawParams = rawParams
	if rawParams != "" {
		sd.ParsedParams = ParseAllSDParams(rawParams)
	}
	sd.GenerationHash = computeGenerationHash(
		sd.Prompt, sd.NegativePrompt, sd.Model, sd.Sampler, sd.Steps, sd.CFGScale,
	)
	return sd
}

func findParamLineIndex(text string) int {
	lines := strings.Split(text, "\n")
	pos := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Steps:") {
			return pos
		}
		pos += len(line) + 1
	}
	return -1
}

// ParseAllSDParams keeps the line's order, and a braced value stays whole.
func ParseAllSDParams(paramLine string) []models.SDParam {
	var result []models.SDParam
	seen := map[string]bool{}
	eachA1111Param(paramLine, func(key, val string) {
		if !seen[key] {
			seen[key] = true
			result = append(result, models.SDParam{Key: key, Val: val})
		}
	})
	return result
}

func splitA1111Params(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, ch := range s {
		switch ch {
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

func eachA1111Param(paramLine string, fn func(key, val string)) {
	paramLine = strings.ReplaceAll(paramLine, "\n", " ")
	for _, part := range splitA1111Params(paramLine) {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		if key == "" {
			continue
		}
		fn(key, strings.TrimSpace(kv[1]))
	}
}

func parseA1111Params(paramLine string, sd *models.SDMetadata) {
	eachA1111Param(paramLine, func(key, val string) {
		switch key {
		case "Steps":
			if n, err := strconv.Atoi(val); err == nil {
				sd.Steps = &n
			}
		case "Sampler":
			sd.Sampler = val
		case "CFG scale":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				sd.CFGScale = &f
			}
		case "Seed":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				sd.Seed = &n
			}
		case "Model":
			if sd.Model == "" {
				sd.Model = val
			} else {
				sd.Model += ", " + val
			}
		case "Lora hashes":
			loraVal := strings.TrimSpace(val)
			if loraVal != "" && loraVal != "{}" {
				if sd.Model != "" {
					sd.Model += " | LoRA: " + loraVal
				} else {
					sd.Model = "LoRA: " + loraVal
				}
			}
		}
	})
}
