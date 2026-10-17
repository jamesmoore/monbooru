package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

const (
	// Bumps only on a breaking change to RelayRequest.
	PayloadVersion = 1
	// No retry: the peer may have committed the work, and a second call
	// would repeat it.
	RelayTimeout = 10 * time.Second
	MessageMax   = 200
)

type RelayRequest struct {
	Payload  int     `json:"payload"`
	Monbooru string  `json:"monbooru"`
	Gallery  string  `json:"gallery"`
	Slot     string  `json:"slot"`
	Button   string  `json:"button"`
	ImageIDs []int64 `json:"image_ids"`
}

// The rows of ids whose media b names; fileTypes reads them.
func Scope(b config.PluginButton, ids []int64, fileTypes func([]int64) (map[int64]string, error)) []int64 {
	if b.Media == "" {
		return ids
	}
	types, err := fileTypes(ids)
	// A partial read knows nothing of the rows it never reached; dropping
	// them would quietly shrink the scope.
	if err != nil {
		logx.Warnf("plugin scope for %s: %v", b.Label, err)
		return ids
	}
	return slices.DeleteFunc(ids, func(id int64) bool {
		fileType, ok := types[id]
		return !ok || !b.AppliesTo(fileType)
	})
}

type RelayAnswer struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Refresh bool   `json:"refresh"`
}

func CallRelay(ctx context.Context, name, target, token string, payload RelayRequest) (RelayAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, RelayTimeout)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return RelayAnswer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return RelayAnswer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := peer.Client.Do(req)
	if err != nil {
		return RelayAnswer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return RelayAnswer{}, peer.StatusError{Peer: name, Status: resp.Status}
	}
	var answer RelayAnswer
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<16)).Decode(&answer); err != nil {
		return RelayAnswer{}, peer.StatusError{Peer: name, Status: "an unreadable answer"}
	}
	return answer, nil
}
