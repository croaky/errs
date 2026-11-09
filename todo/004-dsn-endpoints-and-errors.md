# DSN endpoints and errors

Fixes: `find_01LbYCXgohbWFTBMDu5PWR54` and
`find_01CoHHRzTo6pUmxG1MuvM9nd`.

## Need

`client.perfSender` makes the perf endpoint with
`strings.Replace(c.dsn.endpoint, "/errors/", "/perf/", 1)`. When the
DSN host is `errors`, the first match is in the host, so batches and
the bearer key go to a host named `perf`. A peer on the same network
that answers for `perf` gets the key.

`parseDSN` wraps the `url.Parse` error with `%w`. That error quotes
the whole input, key included. A host that logs the `Init` error, as
the README shows with `log.Fatal(err)`, writes the key to its log
when the DSN is malformed, for example with stray quotes.

## Change

1. `parseDSN` builds both endpoints from the parsed scheme, host,
   repo, and service, and stores them in `dsn.endpoint` and a new
   `dsn.perfEndpoint`. `client.perfSender` reads the field. The
   `strings.Replace` goes away.
2. When `url.Parse` fails, `parseDSN` returns
   `errors.New("parse dsn: not a URL; " + want)`. It does not wrap the
   `url.Parse` error.

## Cost

About 4 lines. Step 2 drops the parse reason, such as "invalid port".
The reason can quote the key in a bad escape, so a cut reason is not
safe. The `want` text still names the expected form.

## Verify

- A DSN with host `errors`, and a DSN with repo or service `errors`,
  give a perf endpoint with the same host and the path
  `/perf/<repo>/<service>`.
- A marker key in DSNs that fail `url.Parse` (quotes, bad port, bad
  escape, control character, bad userinfo character) is absent from
  `err.Error()` and from each error in the unwrap chain.
