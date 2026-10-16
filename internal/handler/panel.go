package handler

// The server side of the panel (`sqlh panel`): subscribers on the control
// socket get events (cache, connection, cursor, focus), panels register the
// editor pane they belong to, and pending actions hand what a panel chose to
// the `sqlh panel take` the editor runs.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/control"
)

// pendingTTL: how long a chosen object waits for `sqlh panel take`.
const pendingTTL = 15 * time.Second

// cursorDebounce: cursor events are sent once the cursor rests this long.
const cursorDebounce = 150 * time.Millisecond

type subscriber struct {
	conn       *jsonrpc2.Conn
	pane       string
	editorPane string
}

type pendingAction struct {
	action control.PanelAction
	at     time.Time
}

// panelState is guarded by its own mutex, not s.mu: events come from the
// cache worker and from requests holding s.mu.
type panelState struct {
	mu      sync.Mutex
	subs    map[*jsonrpc2.Conn]*subscriber
	pending map[string]pendingAction // editor pane → action
	cursor  *control.Cursor
	timer   *time.Timer
	gen     atomic.Int64
}

// subscribe registers conn for events until it disconnects.
func (s *Server) subscribe(conn *jsonrpc2.Conn, p control.SubscribeParams) control.SubscribeResult {
	ps := &s.panel
	ps.mu.Lock()
	if ps.subs == nil {
		ps.subs = map[*jsonrpc2.Conn]*subscriber{}
	}
	ps.subs[conn] = &subscriber{conn: conn, pane: p.Pane, editorPane: p.EditorPane}
	res := control.SubscribeResult{Generation: ps.gen.Load()}
	if ps.cursor != nil {
		c := *ps.cursor
		res.Cursor = &c
	}
	ps.mu.Unlock()
	go func() {
		<-conn.DisconnectNotify()
		ps.mu.Lock()
		delete(ps.subs, conn)
		ps.mu.Unlock()
	}()
	return res
}

// emit sends an event to every subscriber (or only to those in to).
func (s *Server) emit(ev control.Event, to ...*jsonrpc2.Conn) {
	ps := &s.panel
	ev.Generation = ps.gen.Load()
	ps.mu.Lock()
	conns := to
	if len(to) == 0 {
		for c := range ps.subs {
			conns = append(conns, c)
		}
	}
	ps.mu.Unlock()
	for _, c := range conns {
		go func(c *jsonrpc2.Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = c.Notify(ctx, control.EventNotification, ev)
		}(c)
	}
}

// catalogChanged: the cache or the connection changed.
func (s *Server) catalogChanged(kind string) {
	s.panel.gen.Add(1)
	s.emit(control.Event{Kind: kind})
}

func (s *Server) hasSubscribers() bool {
	ps := &s.panel
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.subs) > 0
}

// noteCursor records the editor's cursor (0-based line, character) and sends
// a cursor event once it rests.
func (s *Server) noteCursor(uri string, line, char int) {
	path := uriToPath(uri)
	if path == "" {
		return
	}
	ps := &s.panel
	ps.mu.Lock()
	defer ps.mu.Unlock()
	c := &control.Cursor{File: path, Line: line + 1, Col: char + 1}
	if ps.cursor != nil && *ps.cursor == *c {
		return
	}
	ps.cursor = c
	if len(ps.subs) == 0 {
		return
	}
	if ps.timer != nil {
		ps.timer.Stop()
	}
	ps.timer = time.AfterFunc(cursorDebounce, func() {
		ps.mu.Lock()
		cur := *ps.cursor
		ps.mu.Unlock()
		s.emit(control.Event{Kind: control.EventCursor, Cursor: &cur})
	})
}

// positionMethods carry the editor's cursor in their params.
var positionMethods = map[string]bool{
	"textDocument/completion":        true,
	"textDocument/hover":             true,
	"textDocument/signatureHelp":     true,
	"textDocument/definition":        true,
	"textDocument/typeDefinition":    true,
	"textDocument/references":        true,
	"textDocument/codeAction":        true,
	"textDocument/rename":            true,
	"textDocument/documentHighlight": true,
}

// trackCursor notes the position of a request that has one.
func (s *Server) trackCursor(req *jsonrpc2.Request) {
	if req.Params == nil || !positionMethods[req.Method] {
		return
	}
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position *struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"position"`
		Range *struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
		} `json:"range"`
	}
	if json.Unmarshal(*req.Params, &p) != nil || p.TextDocument.URI == "" {
		return
	}
	switch {
	case p.Position != nil:
		s.noteCursor(p.TextDocument.URI, p.Position.Line, p.Position.Character)
	case p.Range != nil:
		s.noteCursor(p.TextDocument.URI, p.Range.Start.Line, p.Range.Start.Character)
	}
}

// editPosition is where an edit from old to new text ends (0-based line and
// character), i.e. where the cursor is after typing. ok is false when the
// texts are equal.
func editPosition(old, new string) (line, char int, ok bool) {
	if old == new {
		return 0, 0, false
	}
	// the suffix first: typing "where " after "t\n" is then seen before
	// the newline, not after it (repeating a character can be off by one)
	n := min(len(old), len(new))
	suf := 0
	for suf < n && old[len(old)-1-suf] == new[len(new)-1-suf] {
		suf++
	}
	pre := 0
	for pre < n-suf && old[pre] == new[pre] {
		pre++
	}
	end := len(new) - suf
	for end > 0 && end < len(new) && !utf8.RuneStart(new[end]) {
		end-- // inside a multi-byte character
	}
	before := new[:end]
	line = strings.Count(before, "\n")
	char = utf8.RuneCountInString(before[strings.LastIndexByte(before, '\n')+1:])
	return line, char, true
}

// panelFocus asks the panel of an editor pane to show a cursor and focus
// itself.
func (s *Server) panelFocus(p control.PanelFocusParams) control.PanelFocusResult {
	ps := &s.panel
	ps.mu.Lock()
	var target *subscriber
	for _, sub := range ps.subs {
		if sub.editorPane != "" && sub.editorPane == p.EditorPane {
			target = sub
			break
		}
	}
	if p.Cursor != nil {
		c := *p.Cursor
		ps.cursor = &c
	}
	ps.mu.Unlock()
	if target == nil {
		return control.PanelFocusResult{}
	}
	s.emit(control.Event{Kind: control.EventFocus, Cursor: p.Cursor}, target.conn)
	return control.PanelFocusResult{Found: true, Pane: target.pane}
}

func (s *Server) panels() []control.PanelInfo {
	ps := &s.panel
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := []control.PanelInfo{}
	for _, sub := range ps.subs {
		if sub.editorPane != "" {
			out = append(out, control.PanelInfo{Pane: sub.pane, EditorPane: sub.editorPane})
		}
	}
	return out
}

func (s *Server) setPanelAction(p control.PanelActionParams) {
	ps := &s.panel
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.pending == nil {
		ps.pending = map[string]pendingAction{}
	}
	ps.pending[p.EditorPane] = pendingAction{action: p.Action, at: time.Now()}
}

func (s *Server) takePanelAction(p control.PanelTakeParams) control.PanelTakeResult {
	ps := &s.panel
	ps.mu.Lock()
	defer ps.mu.Unlock()
	pa, ok := ps.pending[p.EditorPane]
	delete(ps.pending, p.EditorPane)
	if !ok || time.Since(pa.at) > pendingTTL {
		return control.PanelTakeResult{}
	}
	return control.PanelTakeResult{Found: true, Action: pa.action}
}

// controlCatalog lists the current connection's objects. s.mu must be held.
func (s *Server) controlCatalog() (*control.CatalogResult, error) {
	cache := s.worker.Cache()
	if s.dbConn == nil || s.curDBCfg == nil || cache == nil {
		return nil, errors.New("not connected")
	}
	opt := catalog.Options{Driver: s.curDBCfg.Driver, Lower: s.getConfig().LowercaseIdentifiers}
	res := &control.CatalogResult{
		Connection:    connAlias(s.curDBCfg),
		Driver:        string(s.curDBCfg.Driver),
		Environment:   s.curDBCfg.Env(),
		Database:      s.curDBCfg.DBName,
		Schema:        s.currentSchema(),
		Generation:    s.panel.gen.Load(),
		SamplePolicy:  s.curDBCfg.SamplePolicy(),
		ViewsLoaded:   cache.Relations != nil && cache.Relations.ViewsLoaded,
		ObjectsLoaded: cache.Objects != nil,
		Schemas:       catalog.Tree(cache, opt),
	}
	return res, nil
}
