package errs

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// dsn is the parsed form of a sockeye DSN, the output of
// `soc error dsn create <repo> <service>`:
//
//	https://<key>@<host>/errors/<repo>/<service>
//
// parseDSN builds both endpoints from the parsed parts. A text edit of
// the URL can match "/errors/" in the host, and send the key to a host
// the DSN does not name.
type dsn struct {
	endpoint     string
	perfEndpoint string
	key          string
}

// parseDSN parses a sockeye DSN. The error names the expected form,
// and never quotes the input, because the input holds the key.
func parseDSN(s string) (dsn, error) {
	const want = "want https://<key>@<host>/errors/<repo>/<service>, the output of soc error dsn create"
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		// The url.Parse error quotes the whole input, key included,
		// and a host that logs the Init error writes it to its log.
		return dsn{}, errors.New("parse dsn: not a URL; " + want)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return dsn{}, errors.New("parse dsn: scheme and host are required; " + want)
	}
	if u.User == nil || u.User.Username() == "" {
		return dsn{}, errors.New("parse dsn: the key is missing; " + want)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "errors" || parts[1] == "" || parts[2] == "" {
		return dsn{}, errors.New("parse dsn: the path must be /errors/<repo>/<service>; " + want)
	}
	// The same key posts error events to /errors/<repo>/<service> and
	// request records to /perf/<repo>/<service>.
	return dsn{
		endpoint:     fmt.Sprintf("%s://%s/errors/%s/%s", u.Scheme, u.Host, parts[1], parts[2]),
		perfEndpoint: fmt.Sprintf("%s://%s/perf/%s/%s", u.Scheme, u.Host, parts[1], parts[2]),
		key:          u.User.Username(),
	}, nil
}
