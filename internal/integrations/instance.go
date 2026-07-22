package integrations

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// InstanceLabelFromURI derives a stable instance label from a URI by taking the first host.
func InstanceLabelFromURI(uri string) (string, error) {
	u := strings.TrimSpace(uri)
	if u == "" {
		return "", errors.New("uri is empty")
	}
	if !strings.Contains(u, "://") {
		u = "scheme://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return "", fmt.Errorf("parse uri: %w", err)
	}
	host := strings.TrimSpace(parsed.Host)
	if host == "" {
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
