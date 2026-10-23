package plugins

import (
	"net"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/desktop"
)

// A paused block answers "", so every outbound call short-circuits.
func Base(c *config.Config, p config.PluginConfig) string {
	if p.Paused {
		return ""
	}
	return Address(c, p)
}

func Address(c *config.Config, p config.PluginConfig) string {
	if u := strings.TrimSpace(p.APIURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	if t := c.Auth.Tokens.Paired(p.Name); t != nil {
		return strings.TrimRight(t.PeerURL, "/")
	}
	return ""
}

// The listener, not server.base_url: behind a proxy base_url may not
// answer from inside the child's container, and the child shares the
// listener's host. An unparsable listener leaves base_url.
func CallbackURL(bind, base string) string {
	if _, _, err := net.SplitHostPort(bind); err != nil {
		return base
	}
	return "http://" + desktop.LoopbackAddr(bind)
}
