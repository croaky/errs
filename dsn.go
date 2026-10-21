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
type dsn struct {
	endpoint string
	key      string
}

// parseDSN parses a sockeye DSN. The error names the expected form.
func parseDSN(s string) (dsn, error) {
	const want = "want https://<key>@<host>/errors/<repo>/<service>, the output of soc error dsn create"
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return dsn{}, fmt.Errorf("parse dsn: %w", err)
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
	return dsn{
		endpoint: fmt.Sprintf("%s://%s/errors/%s/%s", u.Scheme, u.Host, parts[1], parts[2]),
		key:      u.User.Username(),
	}, nil
}
