package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type request struct {
	ID       int64
	Command  string
	ActionID []byte
	OutputID []byte
	BodySize int64
}

type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

func serve(c *cache, in io.Reader, out io.Writer) (err error) {
	enc := json.NewEncoder(out)
	// Advertise immediately, before potentially expensive cache maintenance.
	if err := enc.Encode(response{KnownCommands: []string{"get", "put", "close"}}); err != nil {
		return err
	}
	s, err := c.start()
	if err != nil {
		return err
	}
	defer func() {
		if s != nil {
			err = errors.Join(err, s.close())
		}
	}()
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read request: %w", err)
		}
		if len(line) == 1 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			return fmt.Errorf("decode request: %w", err)
		}
		if req.Command == "close" {
			// Cleanup runs before the response; Go closes stdin immediately
			// after receiving it. Do not depend on post-response work.
			err := s.close()
			s = nil
			res := response{ID: req.ID}
			if err != nil {
				res.Err = err.Error()
			}
			return enc.Encode(res)
		}
		if len(req.ActionID) != sha256.Size || (req.Command == "put" && (len(req.OutputID) != sha256.Size || req.BodySize < 0 || req.BodySize > 1<<60)) {
			return errors.New("invalid cache request IDs or body size")
		}
		var res response
		switch req.Command {
		case "get":
			res, err = s.get(req.ActionID)
		case "put":
			body, finish, bodyErr := readBody(reader, req.BodySize)
			if bodyErr != nil {
				return bodyErr
			}
			res, err = s.put(req, body)
			// If storing fails, still drain the current frame before replying.
			if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
				return drainErr
			}
			if finishErr := finish(); finishErr != nil {
				return finishErr
			}
		default:
			err = fmt.Errorf("unsupported command %q", req.Command)
		}
		res.ID = req.ID
		if err != nil {
			res.Err = err.Error()
		}
		if err := enc.Encode(res); err != nil {
			return err
		}
	}
}

// Go sends a raw base64 JSON string, with no escapes. Limit the encoded
// reader so decoding cannot consume the next request. No full-body buffer.
func readBody(r *bufio.Reader, size int64) (io.Reader, func() error, error) {
	if size == 0 {
		return &io.LimitedReader{R: r, N: 0}, func() error { return nil }, nil
	}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, nil, err
		}
		if b == '\n' || b == '\r' || b == ' ' || b == '\t' {
			continue
		}
		if b != '"' {
			return nil, nil, errors.New("expected base64 body string")
		}
		break
	}
	encoded := &io.LimitedReader{R: r, N: (size + 2) / 3 * 4}
	body := base64.NewDecoder(base64.StdEncoding, encoded)
	finish := func() error {
		if encoded.N != 0 {
			return io.ErrUnexpectedEOF
		}
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		if b != '"' {
			return errors.New("missing closing quote after base64 body")
		}
		return nil
	}
	return body, finish, nil
}
