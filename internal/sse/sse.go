// Package sse reads Server-Sent Events streams.
//
// It implements the subset of the SSE specification that model providers
// actually use: "event" and "data" fields, blank-line record separation, and
// comment lines. Retry and last-event-ID fields are parsed and discarded,
// because no provider streaming API uses them for reconnection.
package sse

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// maxEventBytes bounds a single event. Providers send events far smaller than
// this; the limit exists so a malformed or hostile stream cannot exhaust
// memory.
const maxEventBytes = 8 << 20 // 8 MiB

// Event is one decoded SSE record.
type Event struct {
	// Name is the "event:" field, empty when the stream omits it.
	Name string

	// Data is the "data:" payload. Multiple data lines in one record are
	// joined with newlines, per the SSE specification.
	Data []byte
}

// Reader decodes SSE records from a stream.
//
// It is not safe for concurrent use.
type Reader struct {
	sc   *bufio.Scanner
	ev   Event
	err  error
	done bool
}

// NewReader returns a Reader decoding r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventBytes)
	return &Reader{sc: sc}
}

// Next advances to the next event, reporting whether one was decoded.
//
// It returns false at end of stream and on error; check [Reader.Err] to tell
// them apart.
func (r *Reader) Next() bool {
	if r.done {
		return false
	}

	var (
		name strings.Builder
		data []byte
		seen bool
	)

	for r.sc.Scan() {
		line := r.sc.Bytes()

		// A blank line terminates the record. Records with no data field
		// (keep-alive comments, bare event names) are skipped rather than
		// surfaced, since callers cannot act on them.
		if len(bytes.TrimRight(line, "\r")) == 0 {
			if seen {
				r.ev = Event{Name: name.String(), Data: data}
				return true
			}
			name.Reset()
			data = nil
			continue
		}

		line = bytes.TrimRight(line, "\r")

		// Comments keep the connection alive and carry no payload.
		if line[0] == ':' {
			continue
		}

		field, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			// A field name with no colon has an empty value, per the spec.
			field, value = line, nil
		}
		// Exactly one leading space after the colon is part of the framing.
		value = bytes.TrimPrefix(value, []byte(" "))

		switch string(field) {
		case "event":
			name.Reset()
			name.Write(value)
			seen = true
		case "data":
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, value...)
			seen = true
		default:
			// "id" and "retry" are valid but unused by providers.
		}
	}

	r.done = true
	if err := r.sc.Err(); err != nil {
		r.err = err
		return false
	}

	// A final record not followed by a blank line is still a record.
	if seen {
		r.ev = Event{Name: name.String(), Data: data}
		return true
	}
	return false
}

// Event returns the record [Reader.Next] just decoded. It is valid only after
// Next returns true.
func (r *Reader) Event() Event { return r.ev }

// Err returns the error that stopped the stream, or nil if it ended cleanly.
func (r *Reader) Err() error { return r.err }
