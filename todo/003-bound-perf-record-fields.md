# Bound perf record fields

Fixes: `find_01SPoaitRjRALBfBdHoKjk2T` and
`find_01R33icG43Zji1zZ67VDisKE`.

## Need

`perfSender.add` cuts `User`, `Method`, and `Extra`, but not `Route`
or `Pattern`. A host that passes `r.URL.Path` as `Route`, as one
consumer does, lets a remote user queue 5,000 records with paths near
1 MB. sockeye maps a route past 256 bytes to `/{long}`, so the bytes
reach the queue and the batch body and then go to waste.

`perfExtra` puts the whole bad key, or the whole encode error, into
its note. It returns the note before the `perfExtraMax` test, so one
long key makes a record past 2 KiB. sockeye refuses the whole batch
for one bad record, and five such batches open the perf breaker.

## Change

1. In `add`, map a `Route` past 256 bytes to `/{long}`, the label
   sockeye uses. Cut `Pattern` to 256 bytes. Strip the query first.
2. Return `strings.Clone` of each cut value (`route[:i]`, the `User`
   cut), so a queued record holds only the bytes it sends.
3. In `perfExtra`, write notes of fixed size. The bad-key note names
   the rule and not the key. The encode-error note says that the
   value does not encode and gives no error text.

## Cost

About 10 lines and two tests. One record then has a known maximum
size, and so does a batch of 50. The notes get shorter and the code
gets smaller.

## Verify

- A 1 MB `Route` and a 1 MB `Pattern` give a record under 1 KiB, and
  its batch posts with the other records.
- A top-level `Extra` key of many kilobytes, printable and not, gives
  an `extra` value of at most 2 KiB.
