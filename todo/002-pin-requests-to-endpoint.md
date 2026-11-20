# Send requests only to the DSN endpoints

Fixes: `find_01VcuEW9LtaXDRWn6CKcMsLs`.

`claudesec show <id>` prints the attack path and the fix criteria.
Match the finding to this plan by its ID, not by its title.

## Need

`Init` builds `&http.Client{Timeout: 5 * time.Second}` with no
`CheckRedirect` (`errs.go:196-199`). `deliver` (`errs.go:363`) and
`perfSender.post` (`perf.go:458`) call `c.httpClient.Do`. Go then
follows up to 10 redirects. Go keeps `Authorization` when the new
host name is the same or a subdomain, and it ignores the port and the
scheme. `deliver` (`errs.go:405-409`) and `perfSender.send`
(`perf.go:412`) count a `2xx` from the last hop as delivered.

This undoes the host pinning of `parseDSN`, which `c4693e5` added.

Probe at `07a8a84`, public API only: for `301`, `302`, `303`, `307`,
and `308` to another port on the same host, the second server got
`Authorization` on the error path and on the perf path. A `307` or
`308` sent the POST body again. `DroppedRequests` stayed 0. The probe
did not test a subdomain or a redirect from `https` to `http`. Those
cases follow from the standard library rule above.

Threat model, from `todo/README.md`:

- Attacker: the sockeye endpoint, a proxy in front of it, or a peer on
  an `http` DSN.
- Consequence: the DSN key and the event text go to a host that the
  DSN does not name, in clear text after a redirect to `http`. The
  host sends a blind request to an internal address. An event that
  sockeye did not store counts as delivered.
- Boundary: the response that `deliver` and `perfSender.post` read.

## Change

1. In `Init`, copy the client: `cp := *hc`. Set `cp.CheckRedirect` to
   a function that returns `http.ErrUseLastResponse`. Store `&cp` in
   `c.httpClient`. The copy keeps the caller's `Transport` and `Jar`,
   and it does not change the caller's client.
2. A `3xx` then reaches `deliver` and `perfSender.post` as the last
   response. `deliver` already returns false for a status that is not
   `2xx` or transient. `perfSender.send` already drops a batch with
   such a status. Confirm both with tests, and log the `3xx` with no
   `Location` text.
3. In `README.md` and the `Options` comment, state that errs does not
   follow redirects. A caller's `CheckRedirect` does not apply, because
   a redirect can send the key to another host.

## Options

- This plan: one copy in `Init`, so both senders get the rule. Cost:
  a caller's redirect policy does not apply. An endpoint that moves
  needs a new DSN. No CI cost.
- Patch `deliver` and `perfSender.post` apart: two sites today, and a
  third sender can miss the rule.
- Document only, and keep the caller's policy: the default client
  still follows redirects, so the finding stays open.

## Verify

- A test server answers `301`, `302`, `303`, `307`, and `308`. Each
  answer points to another host, another port on the same host, an
  `http` URL on the same host, and a subdomain. Use a `Transport` with
  a `DialContext` that maps the test host names to `httptest`
  servers.
- For each case, on the error path and on the perf path, the second
  server gets no request. The event counts in `c.dropped`, the batch
  counts in `DroppedRequests`, and the breaker records a failure.
- A test with a caller `HTTPClient` that has its own `CheckRedirect`
  shows the same result, and the caller's client value does not
  change.
