package redis

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBulkBytes = 4 << 20

// conn speaks just enough of the Redis wire protocol to authenticate and run
// INFO. Redis client libraries carry connection pools, cluster routing and
// pub/sub the agent has no use for, and the protocol is small enough that
// implementing it costs less than depending on one.
type conn struct {
	net    net.Conn
	reader *bufio.Reader
}

// dialTarget is a parsed connection string.
type dialTarget struct {
	network  string
	address  string
	username string
	password string
	useTLS   bool
	insecure bool
}

// parseURI accepts redis://, rediss:// and unix:// forms, plus a bare host:port.
func parseURI(raw string) (dialTarget, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return dialTarget{}, errors.New("uri is empty")
	}
	if !strings.Contains(trimmed, "://") {
		if strings.HasPrefix(trimmed, "/") {
			return dialTarget{network: "unix", address: trimmed}, nil
		}
		trimmed = "redis://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return dialTarget{}, fmt.Errorf("parse uri: %w", err)
	}

	target := dialTarget{network: "tcp"}
	if parsed.User != nil {
		target.username = parsed.User.Username()
		target.password, _ = parsed.User.Password()
	}

	switch strings.ToLower(parsed.Scheme) {
	case "redis":
	case "rediss":
		target.useTLS = true
	case "unix":
		target.network = "unix"
		target.address = parsed.Path
		if target.address == "" {
			return dialTarget{}, errors.New("unix uri has no socket path")
		}
		return target, nil
	default:
		return dialTarget{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}

	// Hostname() strips the brackets from an IPv6 literal and Port() reports
	// whether one was given, so JoinHostPort can put the brackets back exactly
	// once. Re-joining parsed.Host directly would double them.
	host := parsed.Hostname()
	if host == "" {
		return dialTarget{}, errors.New("uri host is empty")
	}
	port := parsed.Port()
	if port == "" {
		port = "6379"
	}
	target.address = net.JoinHostPort(host, port)
	target.insecure = parsed.Query().Get("tls_skip_verify") == "true"
	return target, nil
}

func dial(ctx context.Context, target dialTarget, timeout time.Duration) (*conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	var raw net.Conn
	var err error
	if target.useTLS {
		raw, err = tls.DialWithDialer(dialer, target.network, target.address, &tls.Config{
			InsecureSkipVerify: target.insecure,
		})
	} else {
		raw, err = dialer.DialContext(ctx, target.network, target.address)
	}
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	c := &conn{net: raw, reader: bufio.NewReader(raw)}
	if target.password != "" {
		args := []string{"AUTH"}
		if target.username != "" {
			args = append(args, target.username)
		}
		args = append(args, target.password)
		if _, err := c.command(timeout, args...); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("auth: %w", err)
		}
	}
	return c, nil
}

func (c *conn) Close() error {
	if c == nil || c.net == nil {
		return nil
	}
	return c.net.Close()
}

// command writes one array command and reads a single reply.
func (c *conn) command(timeout time.Duration, args ...string) (string, error) {
	deadline := time.Now().Add(timeout)
	if err := c.net.SetDeadline(deadline); err != nil {
		return "", err
	}

	var request strings.Builder
	fmt.Fprintf(&request, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&request, "$%d\r\n%s\r\n", len(arg), arg)
	}
	if _, err := c.net.Write([]byte(request.String())); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return c.readReply()
}

func (c *conn) readReply() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty reply")
	}

	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return "", fmt.Errorf("redis error: %s", line[1:])
	case ':':
		return line[1:], nil
	case '$':
		size, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", fmt.Errorf("bad bulk length %q", line[1:])
		}
		if size < 0 {
			return "", nil
		}
		if size > maxBulkBytes {
			return "", fmt.Errorf("bulk reply of %d bytes exceeds limit", size)
		}
		buf := make([]byte, size+2)
		if _, err := readFull(c.reader, buf); err != nil {
			return "", fmt.Errorf("read bulk: %w", err)
		}
		return string(buf[:size]), nil
	default:
		return "", fmt.Errorf("unsupported reply type %q", line[0])
	}
}

func readFull(reader *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := reader.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// parseInfo turns the INFO reply into a flat map. Section headers start with '#'.
func parseInfo(raw string) map[string]string {
	out := make(map[string]string, 128)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
