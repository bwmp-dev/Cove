package worker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"
)

type TerminalRequest struct {
	ID      string `json:"id,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Offset  int64  `json:"offset"`
	Columns uint16 `json:"columns"`
	Rows    uint16 `json:"rows"`
}

type TerminalResponse struct {
	ID     string `json:"id"`
	Data   []byte `json:"data"`
	Offset int64  `json:"offset"`
	Closed bool   `json:"closed"`
}

type terminalSession struct {
	mu        sync.Mutex
	file      *os.File
	resize    func(uint16, uint16) error
	stop      func()
	data      []byte
	base      int64
	closed    bool
	lastInput time.Time
	created   time.Time
}

func (e *Engine) terminal(method string, in *TerminalRequest) (*TerminalResponse, error) {
	if in == nil || len(in.Data) > 32768 || in.Columns > 500 || in.Rows > 300 {
		return nil, errors.New("invalid terminal request")
	}
	e.terminalMu.Lock()
	defer e.terminalMu.Unlock()
	if e.terminals == nil {
		e.terminals = map[string]*terminalSession{}
	}
	if method == "terminal_open" {
		if len(e.terminals) >= 8 {
			return nil, errors.New("node terminal limit reached")
		}
		if in.Columns < 2 || in.Rows < 2 {
			return nil, errors.New("invalid terminal size")
		}
		file, resize, stop, err := startTerminal(e.Root, in.Columns, in.Rows)
		if err != nil {
			return nil, errors.New("could not open node shell")
		}
		var key [32]byte
		if _, err = rand.Read(key[:]); err != nil {
			stop()
			return nil, err
		}
		id := hex.EncodeToString(key[:])
		s := &terminalSession{file: file, resize: resize, stop: stop, lastInput: time.Now(), created: time.Now()}
		e.terminals[id] = s
		go func() {
			buffer := make([]byte, 8192)
			for {
				n, err := file.Read(buffer)
				s.mu.Lock()
				if len(s.data)+n > 256*1024 {
					s.closed = true
					s.stop()
					s.mu.Unlock()
					return
				}
				s.data = append(s.data, buffer[:n]...)
				if err != nil {
					s.closed = true
					s.mu.Unlock()
					return
				}
				s.mu.Unlock()
			}
		}()
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				e.terminalMu.Lock()
				current, ok := e.terminals[id]
				if !ok {
					e.terminalMu.Unlock()
					return
				}
				current.mu.Lock()
				expired := time.Since(current.lastInput) > 5*time.Minute || time.Since(current.created) > time.Hour
				if expired {
					current.stop()
					delete(e.terminals, id)
				}
				current.mu.Unlock()
				e.terminalMu.Unlock()
				if expired {
					return
				}
			}
		}()
		return &TerminalResponse{ID: id}, nil
	}
	s, ok := e.terminals[in.ID]
	if !ok {
		return nil, errors.New("terminal session expired")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastInput = time.Now()
	switch method {
	case "terminal_close":
		s.stop()
		delete(e.terminals, in.ID)
		return &TerminalResponse{ID: in.ID, Closed: true}, nil
	case "terminal_write":
		if s.closed {
			return nil, errors.New("terminal session ended")
		}
		if len(in.Data) > 0 {
			timeout := time.AfterFunc(2*time.Second, s.stop)
			_, err := s.file.Write(in.Data)
			timeout.Stop()
			if err != nil {
				return nil, errors.New("terminal input failed")
			}
		}
		if in.Columns >= 2 && in.Rows >= 2 {
			if err := s.resize(in.Columns, in.Rows); err != nil {
				return nil, err
			}
		}
	case "terminal_read":
		if in.Offset < s.base || in.Offset > s.base+int64(len(s.data)) {
			return nil, errors.New("invalid terminal output offset")
		}
		s.data = append([]byte(nil), s.data[in.Offset-s.base:]...)
		s.base = in.Offset
	default:
		return nil, errors.New("unsupported terminal method")
	}
	result := &TerminalResponse{ID: in.ID, Closed: s.closed, Offset: s.base}
	if method == "terminal_read" {
		result.Data = append([]byte(nil), s.data...)
		result.Offset += int64(len(s.data))
	}
	return result, nil
}
