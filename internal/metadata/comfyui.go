package metadata

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
)

// Python's json writes NaN and Infinity, which encoding/json refuses;
// they become null outside strings.
func sanitizeComfyJSON(s string) string {
	if !strings.Contains(s, "NaN") && !strings.Contains(s, "Infinity") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			b.WriteByte(c)
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		switch {
		case strings.HasPrefix(s[i:], "-Infinity"):
			b.WriteString("null")
			i += len("-Infinity") - 1
		case strings.HasPrefix(s[i:], "Infinity"):
			b.WriteString("null")
			i += len("Infinity") - 1
		case strings.HasPrefix(s[i:], "NaN"):
			b.WriteString("null")
			i += len("NaN") - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func parseComfyPromptChunk(raw string) *models.ComfyUIMetadata {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = sanitizeComfyJSON(raw)

	// API format: {"1": {"inputs": {...}, "class_type": "...", "_meta": {...}}, ...}
	var apiNodes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &apiNodes); err != nil {
		return nil
	}

	parsedNodes := make(map[string]comfyAPINode, len(apiNodes))
	for id, nodeRaw := range apiNodes {
		var node comfyAPINode
		if err := json.Unmarshal(nodeRaw, &node); err == nil {
			parsedNodes[id] = node
		}
	}

	meta := &models.ComfyUIMetadata{}
	nodeIDs := comfyNodeIDs(parsedNodes)

	// The KSampler's positive input says which CLIPTextEncode is the prompt.
	positiveNodeID := ""
	for _, id := range nodeIDs {
		node := parsedNodes[id]
		nodeType := node.typeName()
		if nodeType == "KSampler" || nodeType == "KSamplerAdvanced" {
			if posRaw, ok := node.Inputs["positive"]; ok {
				var ref []json.RawMessage
				if err := json.Unmarshal(posRaw, &ref); err == nil && len(ref) >= 1 {
					var nid string
					if err := json.Unmarshal(ref[0], &nid); err == nil {
						positiveNodeID = nid
					}
				}
			}
			break
		}
	}

	for _, id := range nodeIDs {
		node := parsedNodes[id]
		nodeType := node.typeName()
		if nodeType == "CLIPTextEncode" {
			if textRaw, ok := node.Inputs["text"]; ok {
				text := resolveStringInput(textRaw, parsedNodes)
				if text != "" {
					if positiveNodeID != "" {
						if id == positiveNodeID {
							meta.Prompt = text
						}
					} else if meta.Prompt == "" {
						meta.Prompt = text
					}
				}
			}
			continue
		}
		applyComfyNodeInputs(nodeType, node.Inputs, meta, parsedNodes)
	}

	if meta.Prompt == "" {
		for _, id := range nodeIDs {
			node := parsedNodes[id]
			nodeType := node.typeName()
			if nodeType == "PrimitiveStringMultiline" {
				if valRaw, ok := node.Inputs["value"]; ok {
					var val string
					if err := json.Unmarshal(valRaw, &val); err == nil && val != "" && len(val) > 10 {
						meta.Prompt = val
						break
					}
				}
			}
		}
	}

	// A graph the extractors all pass over is still ComfyUI: its nodes
	// stay searchable and only the recipe columns are empty.
	if !slices.ContainsFunc(nodeIDs, func(id string) bool { return parsedNodes[id].typeName() != "" }) {
		return nil
	}
	meta.RawWorkflow = raw
	if meta.Prompt != "" || meta.ModelCheckpoint != "" || meta.Seed != nil {
		meta.GenerationHash = computeGenerationHash(
			meta.Prompt, "", meta.ModelCheckpoint, meta.Sampler, meta.Steps, meta.CFGScale,
		)
	}
	return meta
}

type comfyGraphNode struct {
	Key       string
	ClassType string
	Title     string
	Inputs    map[string]json.RawMessage
}

// The node-graph shape lists nodes under "nodes"; the API shape keys them
// by id at the top level, where links, extra and version have no class
// and drop out.
func comfyGraphNodes(raw string) []comfyGraphNode {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sanitizeComfyJSON(raw)), &top); err != nil {
		return nil
	}
	if nodesRaw, ok := top["nodes"]; ok {
		var entries []json.RawMessage
		if err := json.Unmarshal(nodesRaw, &entries); err == nil {
			nodes := make([]comfyGraphNode, 0, len(entries))
			for _, entry := range entries {
				if node, ok := decodeComfyGraphNode(entry); ok && node.ClassType != "" {
					nodes = append(nodes, node)
				}
			}
			return nodes
		}
	}
	nodes := make([]comfyGraphNode, 0, len(top))
	for _, key := range comfyNodeIDs(top) {
		node, ok := decodeComfyGraphNode(top[key])
		if !ok || node.ClassType == "" {
			continue
		}
		node.Key = key
		nodes = append(nodes, node)
	}
	return nodes
}

func decodeComfyGraphNode(raw json.RawMessage) (comfyGraphNode, bool) {
	var n struct {
		ID        json.RawMessage `json:"id"`
		ClassType string          `json:"class_type"`
		Type      string          `json:"type"`
		Title     string          `json:"title"`
		Meta      struct {
			Title string `json:"title"`
		} `json:"_meta"`
		Inputs json.RawMessage `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return comfyGraphNode{}, false
	}
	node := comfyGraphNode{
		Key:       strings.Trim(string(n.ID), `"`),
		ClassType: cmp.Or(n.ClassType, n.Type),
		Title:     cmp.Or(n.Meta.Title, n.Title),
	}
	// Node-graph inputs are link descriptors with no values: they read as
	// none rather than failing the node.
	_ = json.Unmarshal(n.Inputs, &node.Inputs)
	return node, true
}

// Node-graph values sit in a positional widgets_values array that means
// nothing without the node definitions, so the recipe is read only where
// inputs are named.
func parseComfyWorkflow(raw string) *models.ComfyUIMetadata {
	nodes := comfyGraphNodes(raw)
	if len(nodes) == 0 {
		return nil
	}
	meta := &models.ComfyUIMetadata{RawWorkflow: sanitizeComfyJSON(strings.TrimSpace(raw))}
	for _, node := range nodes {
		applyComfyNodeInputs(node.ClassType, node.Inputs, meta, nil)
	}
	if meta.Prompt != "" || meta.ModelCheckpoint != "" || meta.Seed != nil {
		meta.GenerationHash = computeGenerationHash(
			meta.Prompt, "", meta.ModelCheckpoint, meta.Sampler, meta.Steps, meta.CFGScale,
		)
	}
	return meta
}

type comfyAPINode struct {
	ClassType string                     `json:"class_type"`
	Type      string                     `json:"type"`
	Inputs    map[string]json.RawMessage `json:"inputs"`
}

func (n comfyAPINode) typeName() string {
	if n.ClassType != "" {
		return n.ClassType
	}
	return n.Type
}

// A reference input is a [nodeID, slotIndex] array.
func resolveRefInput[T any](raw json.RawMessage, nodes map[string]comfyAPINode, candidates []string, keep func(T) bool) (T, bool) {
	var zero, v T
	if err := json.Unmarshal(raw, &v); err == nil {
		return v, true
	}
	var ref []json.RawMessage
	if err := json.Unmarshal(raw, &ref); err != nil || len(ref) < 1 {
		return zero, false
	}
	var nodeID string
	if err := json.Unmarshal(ref[0], &nodeID); err != nil {
		return zero, false
	}
	refNode, ok := nodes[nodeID]
	if !ok {
		return zero, false
	}
	for _, key := range candidates {
		if valRaw, ok := refNode.Inputs[key]; ok {
			var val T
			if err := json.Unmarshal(valRaw, &val); err == nil && keep(val) {
				return val, true
			}
		}
	}
	return zero, false
}

func resolveStringInput(raw json.RawMessage, nodes map[string]comfyAPINode) string {
	s, _ := resolveRefInput(raw, nodes, []string{"value", "text", "string"}, func(v string) bool { return v != "" })
	return s
}

func resolveInt64Input(raw json.RawMessage, nodes map[string]comfyAPINode) (int64, bool) {
	return resolveRefInput(raw, nodes, []string{"seed", "value", "int"}, func(int64) bool { return true })
}

// ParseComfyWorkflowNodes keeps nodes with no inputs so the graph stays
// complete: a node-graph workflow has none to show.
func ParseComfyWorkflowNodes(raw string) []models.ComfyNode {
	graph := comfyGraphNodes(raw)
	nodes := make([]models.ComfyNode, 0, len(graph))
	for _, n := range graph {
		paramKeys := make([]string, 0, len(n.Inputs))
		for pk := range n.Inputs {
			paramKeys = append(paramKeys, pk)
		}
		slices.Sort(paramKeys)

		var params []models.ComfyNodeParam
		for _, pk := range paramKeys {
			if param := comfyParamToDisplay(pk, n.Inputs[pk]); param != nil {
				params = append(params, *param)
			}
		}

		nodes = append(nodes, models.ComfyNode{
			Key:        n.Key,
			Title:      cmp.Or(n.Title, n.ClassType),
			ClassType:  n.ClassType,
			Params:     params,
			SearchTerm: comfyTerm("node", n.ClassType),
		})
	}
	return nodes
}

// The display and the term index share this, so a shown value and its
// term cannot drift.
func comfyScalar(raw json.RawMessage) (string, bool) {
	if string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	// float64 rounds an integer past 2^53.
	if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
		return strconv.FormatInt(n, 10), true
	}
	if n, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
		return strconv.FormatUint(n, 10), true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f == float64(int64(f)) {
			return fmt.Sprintf("%d", int64(f)), true
		}
		return fmt.Sprintf("%g", f), true
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b), true
	}
	return "", false
}

func comfyParamToDisplay(name string, raw json.RawMessage) *models.ComfyNodeParam {
	if string(raw) == "null" {
		return nil
	}
	if val, ok := comfyScalar(raw); ok {
		return &models.ComfyNodeParam{Name: name, Value: val, SearchTerm: comfyLinkTerm(name, val)}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) >= 1 {
		var ref string
		if err := json.Unmarshal(arr[0], &ref); err == nil {
			return &models.ComfyNodeParam{Name: name, Value: "→ " + ref, IsRef: true}
		}
	}
	return &models.ComfyNodeParam{Name: name, Value: string(raw)}
}

// A fixed order: the passes are first-writer-wins, so map order would
// change the checkpoint, LoRA order and hash between parses.
func comfyNodeIDs[T any](nodes map[string]T) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sortComfyKeys(ids)
	return ids
}

func sortComfyKeys(keys []string) {
	slices.SortFunc(keys, func(a, b string) int {
		ai, bi := parseIntKey(a), parseIntKey(b)
		if ai >= 0 && bi >= 0 {
			return cmp.Compare(ai, bi)
		}
		if ai >= 0 {
			return -1
		}
		if bi >= 0 {
			return 1
		}
		return cmp.Compare(a, b)
	})
}

func parseIntKey(s string) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func nodeInput[T any](inputs map[string]json.RawMessage, key string) (T, bool) {
	var v T
	raw, ok := inputs[key]
	if !ok {
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, false
	}
	return v, true
}

func applyComfyNodeInputs(nodeType string, inputs map[string]json.RawMessage, meta *models.ComfyUIMetadata, nodes map[string]comfyAPINode) {
	switch nodeType {
	case "CLIPTextEncode":
		if textRaw, ok := inputs["text"]; ok && meta.Prompt == "" {
			text := resolveStringInput(textRaw, nodes)
			if text != "" {
				meta.Prompt = text
			}
		}
	case "KSampler", "KSamplerAdvanced", "KSamplerSelect":
		if seedRaw, ok := inputs["seed"]; ok && meta.Seed == nil {
			if seed, ok := resolveInt64Input(seedRaw, nodes); ok {
				meta.Seed = &seed
			}
		}
		if steps, ok := nodeInput[int](inputs, "steps"); ok && meta.Steps == nil {
			meta.Steps = &steps
		}
		if cfg, ok := nodeInput[float64](inputs, "cfg"); ok && meta.CFGScale == nil {
			meta.CFGScale = &cfg
		}
		if sampler, ok := nodeInput[string](inputs, "sampler_name"); ok && meta.Sampler == "" {
			meta.Sampler = sampler
		}
		if scheduler, ok := nodeInput[string](inputs, "scheduler"); ok && scheduler != "" && meta.Sampler != "" {
			meta.Sampler += "/" + scheduler
		}
	case "CheckpointLoaderSimple", "CheckpointLoader", "unCLIPCheckpointLoader":
		if ckpt, ok := nodeInput[string](inputs, "ckpt_name"); ok && meta.ModelCheckpoint == "" {
			meta.ModelCheckpoint = ckpt
		}
	case "UNETLoader":
		if unet, ok := nodeInput[string](inputs, "unet_name"); ok && meta.ModelCheckpoint == "" {
			meta.ModelCheckpoint = unet
		}
	case "LoraLoader", "LoraLoaderModelOnly":
		if lora, ok := nodeInput[string](inputs, "lora_name"); ok && lora != "" {
			appendCheckpoint(meta, lora)
		}
	case "Lora Loader Stack (rgthree)":
		for i := 1; i <= 10; i++ {
			key := fmt.Sprintf("lora_%02d", i)
			if lora, ok := nodeInput[string](inputs, key); ok && lora != "" && lora != "None" {
				appendCheckpoint(meta, lora)
			}
		}
	case "Seed (rgthree)", "SeedNode", "RandomSeed":
		if seed, ok := nodeInput[int64](inputs, "seed"); ok && meta.Seed == nil {
			meta.Seed = &seed
		}
	case "easy fullLoader", "easy a1111Loader":
		if ckpt, ok := nodeInput[string](inputs, "ckpt_name"); ok && meta.ModelCheckpoint == "" {
			meta.ModelCheckpoint = ckpt
		}
	}
}

func appendCheckpoint(meta *models.ComfyUIMetadata, name string) {
	if meta.ModelCheckpoint != "" {
		meta.ModelCheckpoint += " + " + name
		return
	}
	meta.ModelCheckpoint = name
}
