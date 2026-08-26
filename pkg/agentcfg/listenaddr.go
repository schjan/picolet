package agentcfg

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// defaultMetricsPort is the port the HTTP server binds when neither
	// listen_addr nor metrics_port names one.
	defaultMetricsPort = 9417
	// unsetPort is metrics_port's long-standing "use the default" sentinel; it
	// also excludes the port from the listen_addr agreement check.
	unsetPort = 0
	// loopbackHost keeps a fresh install off every non-loopback interface:
	// /metrics, /health, /webhook and the dashboard are unauthenticated.
	loopbackHost = "127.0.0.1"
	maxPort      = 65535
)

// EffectiveListenAddr returns the host:port the agent's HTTP server binds.
// listen_addr wins when it carries a port; a portless listen_addr takes its
// port from metrics_port; an unset listen_addr binds loopback.
func (c *Config) EffectiveListenAddr() string {
	host, port := c.listenHostPort()
	return net.JoinHostPort(host, port)
}

// DialAddr returns the address a client on the same Machine (bootstrap health
// probe, `picolet healthcheck`, `picolet trigger`) must dial to reach the
// agent: the effective listen address with a wildcard host rewritten to
// loopback, since a wildcard is not a routable destination.
func (c *Config) DialAddr() string {
	host, port := c.listenHostPort()
	return net.JoinHostPort(dialHost(host), port)
}

// ListensOnLoopback reports whether the effective listen address is reachable
// from the local network namespace only.
func (c *Config) ListensOnLoopback() bool {
	host, _ := c.listenHostPort()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) listenHostPort() (host, port string) {
	host, port = splitListenAddr(c.ListenAddr)
	if port == "" {
		port = strconv.Itoa(c.metricsPort())
	}
	return host, port
}

func (c *Config) metricsPort() int {
	if c.MetricsPort == unsetPort {
		return defaultMetricsPort
	}
	return c.MetricsPort
}

// splitListenAddr splits listen_addr into host and port. An unset addr means
// loopback; an addr carrying no port yields an empty port for the caller to
// fill from metrics_port.
func splitListenAddr(addr string) (host, port string) {
	if addr == "" {
		return loopbackHost, ""
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return addr, ""
}

// dialHost rewrites a wildcard bind to loopback: a wildcard is not a routable
// destination. An IPv6 wildcard accepts IPv4-mapped connections, so one
// loopback address serves both.
func dialHost(host string) string {
	switch host {
	case "", "0.0.0.0", "::":
		return loopbackHost
	}
	return host
}

// validateListenAddr enforces that listen_addr parses and that metrics_port,
// when set alongside a listen_addr carrying its own port, agrees with it — the
// two must never name different ports.
func (c *Config) validateListenAddr() error {
	if !validMetricsPort(c.MetricsPort) {
		return fmt.Errorf("metrics_port must be between 0 and %d: %d", maxPort, c.MetricsPort)
	}
	if c.ListenAddr == "" {
		return nil
	}
	host, port := splitListenAddr(c.ListenAddr)
	if err := validateListenHost(host); err != nil {
		return err
	}
	if port == "" {
		return nil
	}
	return c.validateListenPort(port)
}

// validateListenPort requires a decimal port a client can actually dial:
// ParseUint rejects signs and non-decimal digits that net.Listen would accept
// but net/url would later choke on, and port 0 is refused because nothing can
// dial the ephemeral port the kernel picks.
func (c *Config) validateListenPort(port string) error {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return fmt.Errorf("listen_addr port must be a decimal port between 1 and %d: %q", maxPort, c.ListenAddr)
	}
	if n == unsetPort {
		return fmt.Errorf("listen_addr port must not be 0: %q would bind an ephemeral port no client can find", c.ListenAddr)
	}
	if c.MetricsPort != unsetPort && c.MetricsPort != int(n) {
		return fmt.Errorf("listen_addr port %d disagrees with metrics_port %d; set only one", n, c.MetricsPort)
	}
	return nil
}

// validateListenHost accepts an empty host (all interfaces), any IP literal and
// any name free of address-syntax characters. A zone suffix (fe80::1%eth0) is
// refused with them: every local consumer builds a URL from the address, and
// net/url rejects the unescaped zone, so binding one would only move the
// failure from start-up to the first probe.
func validateListenHost(host string) error {
	if host == "" || net.ParseIP(host) != nil {
		return nil
	}
	if strings.ContainsAny(host, ":/% ") {
		return fmt.Errorf("listen_addr must be host:port or a host: %q", host)
	}
	return nil
}

// validMetricsPort allows 0 — that is the unset sentinel, not a port.
func validMetricsPort(port int) bool {
	return port >= unsetPort && port <= maxPort
}
