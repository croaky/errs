package errs

// journalPrefix is the explicit event marker understood by Sockeye. A raw
// ERROR line or panic stack is not an event. No source name or credential
// is added here: trusted host policy binds the console unit to its service.
const journalPrefix = "sockeye.error.v1 "
const journalBodyMax = 256 << 10

// journalResult classifies one journal write attempt. Size refusals
// belong to one event and must not open the shared circuit breaker.
type journalResult uint8

const (
	journalOK journalResult = iota
	journalTooLarge
	journalWriteFailed
)

// writeJournal attempts one complete frame with one Write. It never retries
// a partial write, which could splice two JSON values into one console line.
// It does not close a writer that belongs to the host. Success proves only
// that the writer accepted the bytes, not that a journal or server kept them.
func (c *client) writeJournal(body []byte) journalResult {
	if len(body) > journalBodyMax {
		logger.Printf("errs: journal event is %d bytes; limit %d", len(body), journalBodyMax)
		return journalTooLarge
	}
	frame := make([]byte, 0, len(journalPrefix)+len(body)+1)
	frame = append(frame, journalPrefix...)
	frame = append(frame, body...)
	frame = append(frame, '\n')
	n, err := c.journal.Write(frame)
	if err != nil || n != len(frame) {
		logger.Print("errs: journal event write failed")
		return journalWriteFailed
	}
	return journalOK
}
