package integrations

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

// InstanceLabelFromURI derives a stable instance label from a URI by taking the
// first host. A service reached over a unix socket has no host, so the socket
// path names it instead — PostgreSQL, MySQL, MongoDB and Redis are all commonly
// connected to that way on the machine they run on, and refusing those URIs
// would stop the agent rather than one integration.
func InstanceLabelFromURI(uri string) (string, error) {
	u := strings.TrimSpace(uri)
	if u == "" {
		return "", errors.New("uri is empty")
	}
	if !strings.Contains(u, "://") {
		if strings.HasPrefix(u, "/") {
			return labelFromSocketPath(u)
		}
		u = "scheme://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return "", fmt.Errorf("parse uri: %w", err)
	}
	host := strings.TrimSpace(parsed.Host)
	if host == "" {
		// Either a socket URI (unix:///run/redis.sock) or the libpq form that
		// carries the socket directory in the query (postgres:///db?host=/run/postgresql).
		if hostParam := strings.TrimSpace(parsed.Query().Get("host")); hostParam != "" {
			if strings.HasPrefix(hostParam, "/") {
				return labelFromSocketPath(hostParam)
			}
			return hostnameFromHost(hostParam), nil
		}
		if path := strings.TrimSpace(parsed.Path); path != "" {
			return labelFromSocketPath(path)
		}
		return "", errors.New("uri host is empty")
	}
	if idx := strings.IndexByte(host, ','); idx >= 0 {
		host = strings.TrimSpace(host[:idx])
	}
	host = strings.TrimSpace(hostnameFromHost(host))
	if host == "" {
		return "", errors.New("uri host is empty")
	}
	return host, nil
}

// labelFromSocketPath names an instance after its socket, so
// "/var/run/redis.sock" reads as "redis" rather than as a path.
func labelFromSocketPath(socketPath string) (string, error) {
	base := strings.TrimSpace(path.Base(strings.TrimRight(socketPath, "/")))
	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("socket path %q has no name", socketPath)
	}
	if ext := path.Ext(base); ext != "" && ext != base {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		return "", fmt.Errorf("socket path %q has no name", socketPath)
	}
	return base, nil
}

func hostnameFromHost(host string) string {
	if host == "" {
		return host
	}
	if strings.HasPrefix(host, "[") {
		if h, _, err := net.SplitHostPort(host); err == nil {
			return h
		}
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		return host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	if strings.Count(host, ":") == 1 {
		parts := strings.Split(host, ":")
		if len(parts) == 2 {
			return parts[0]
		}
	}
	return host
}
