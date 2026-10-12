package errs

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// dsn is the parsed form of a Sentry DSN:
//
//	{scheme}://{publicKey}@{host}[:{port}]{path}/{projectID}
//
// path is the joined leading path segments. Project id is the last
// segment.
type dsn struct {
	scheme    string
	host      string
	port      string // Port number; empty means the default port for the scheme.
	publicKey string
	projectID string
	path      string // Path with leading "/", or empty.
}

// parseDSN parses a Sentry DSN string into a dsn struct.
// It returns an error when required components are missing.
func parseDSN(s string) (dsn, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return dsn{}, fmt.Errorf("parse dsn: %w", err)
	}
	if u.Scheme == "" || u.Host == "" || u.User == nil {
		return dsn{}, errors.New("parse dsn: scheme, host, and public key are required")
	}
	publicKey := u.User.Username()
	if publicKey == "" {
		return dsn{}, errors.New("parse dsn: missing public key")
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return dsn{}, errors.New("parse dsn: missing project id")
	}
	projectID := parts[len(parts)-1]
	leading := parts[:len(parts)-1]

	path := ""
	if len(leading) > 0 {
		path = "/" + strings.Join(leading, "/")
	}

	return dsn{
		scheme:    u.Scheme,
		host:      u.Hostname(),
		port:      u.Port(),
		publicKey: publicKey,
		projectID: projectID,
		path:      path,
	}, nil
}

// envelopeEndpoint returns the URL for envelope POST requests.
// It omits the port when the port matches the scheme default.
func (d dsn) envelopeEndpoint() string {
	port := ""
	if d.port != "" && !isDefaultPort(d.scheme, d.port) {
		port = ":" + d.port
	}
	return fmt.Sprintf("%s://%s%s%s/api/%s/envelope/",
		d.scheme, d.host, port, d.path, d.projectID)
}

// authHeader returns the X-Sentry-Auth header value.
// now is the Unix timestamp in seconds; client identifies this library.
func (d dsn) authHeader(now int64, client string) string {
	return fmt.Sprintf(
		"Sentry sentry_version=%s, sentry_timestamp=%d, sentry_key=%s, sentry_client=%s",
		protocolVersion, now, d.publicKey, client,
	)
}

func isDefaultPort(scheme, port string) bool {
	switch scheme {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	}
	return false
}
