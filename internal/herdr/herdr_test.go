package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeHerdr answers socket requests from a table and records them.
type fakeHerdr struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func startFake(t *testing.T, answer func(method string, params map[string]any) (any, *Error)) (*Client, *fakeHerdr) {
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
	f := &fakeHerdr{}
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
				f.mu.Lock()
				f.reqs = append(f.reqs, req)
				f.mu.Unlock()
				params, _ := req["params"].(map[string]any)
				res, herr := answer(req["method"].(string), params)
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
	return &Client{Socket: sock}, f
}

func TestClient(t *testing.T) {
	cl, f := startFake(t, func(method string, p map[string]any) (any, *Error) {
		switch method {
		case "pane.get":
			if p["pane_id"] == "w1:p1" {
				return map[string]any{"pane": map[string]any{"pane_id": "w1:p1", "workspace_id": "w1"}}, nil
			}
			return nil, &Error{Code: "pane_not_found", Message: "pane not found"}
		case "pane.process_info":
			return map[string]any{"process_info": map[string]any{"foreground_process_group_id": 10,
				"foreground_processes": []any{map[string]any{"pid": 10, "name": "hx", "argv0": "/opt/homebrew/bin/hx"}}}}, nil
		case "pane.split":
			return map[string]any{"pane": map[string]any{"pane_id": "w1:p9"}}, nil
		}
		return map[string]any{}, nil
	})
	p, err := cl.Pane("w1:p1")
	if err != nil || p.WorkspaceID != "w1" {
		t.Fatalf("pane %+v %v", p, err)
	}
	if _, err := cl.Pane("w1:p2"); !NotFound(err) {
		t.Errorf("missing pane: %v", err)
	}
	info, err := cl.ProcessInfo("w1:p1")
	if err != nil || !info.HasProcess("hx", 0) || !info.HasProcess("hx", 10) || info.HasProcess("hx", 11) || info.HasProcess("nu", 0) {
		t.Errorf("process info %+v %v", info, err)
	}
	id, err := cl.Split(SplitOptions{Target: "w1:p1", Direction: "right", Ratio: 0.5, Env: map[string]string{"A": "b"}, Focus: true})
	if err != nil || id != "w1:p9" {
		t.Errorf("split %q %v", id, err)
	}
	if err := cl.SendKeys("w1:p1", "esc", "space", "i", "P"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Focus("w1:p1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	split := f.reqs[3]["params"].(map[string]any)
	if split["target_pane_id"] != "w1:p1" || split["direction"] != "right" || split["ratio"] != 0.5 || split["focus"] != true ||
		split["env"].(map[string]any)["A"] != "b" {
		t.Errorf("split params %+v", split)
	}
	keys := f.reqs[4]["params"].(map[string]any)["keys"].([]any)
	if len(keys) != 4 || keys[1] != "space" {
		t.Errorf("keys %v", keys)
	}
	if f.reqs[5]["method"] != "pane.focus" {
		t.Errorf("focus %+v", f.reqs[5])
	}
}
