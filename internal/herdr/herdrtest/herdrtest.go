// Package herdrtest is a fake Herdr socket for tests: no test may touch a
// live Herdr.
package herdrtest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/raikhan/sqlh/internal/herdr"
)

// Fake records requests.
type Fake struct {
	mu   sync.Mutex
	reqs []Request
}

// Request is a recorded call.
type Request struct {
	Method string
	Params map[string]any
}

// Requests returns the calls so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// Answer answers a method; a nil error and nil result answer {}.
type Answer func(method string, params map[string]any) (any, *herdr.Error)

// Start serves a fake Herdr socket until the test ends.
func Start(t *testing.T, answer Answer) (*herdr.Client, *Fake) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f := &Fake{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				_ = json.Unmarshal(line, &req)
				params, _ := req["params"].(map[string]any)
				method, _ := req["method"].(string)
				f.mu.Lock()
				f.reqs = append(f.reqs, Request{method, params})
				f.mu.Unlock()
				res, herr := answer(method, params)
				if res == nil {
					res = map[string]any{}
				}
				resp := map[string]any{"id": req["id"]}
				if herr != nil {
					resp["error"] = herr
				} else {
					resp["result"] = res
				}
				b, _ := json.Marshal(resp)
				_, _ = c.Write(append(b, '\n'))
			}(c)
		}
	}()
	return &herdr.Client{Socket: sock}, f
}
