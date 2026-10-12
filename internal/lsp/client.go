package lsp

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

type MessageDisplayer interface {
	ShowLog(context.Context, string) error
	ShowInfo(context.Context, string) error
	ShowWarning(context.Context, string) error
	ShowError(context.Context, string) error
	ShowDocument(uri string, takeFocus bool)
}

type Messenger struct {
	conn *jsonrpc2.Conn
}

func NewMessenger(conn *jsonrpc2.Conn) MessageDisplayer {
	return &Messenger{
		conn: conn,
	}
}

func (m *Messenger) ShowLog(ctx context.Context, message string) error {
	log.Println("Send Message:", message)
	params := &ShowMessageParams{
		Type:    Log,
		Message: message,
	}
	return m.conn.Notify(ctx, "window/showMessage", params)
}

func (m *Messenger) ShowInfo(ctx context.Context, message string) error {
	log.Println("Send Message:", message)
	params := &ShowMessageParams{
		Type:    Info,
		Message: message,
	}
	return m.conn.Notify(ctx, "window/showMessage", params)
}

func (m *Messenger) ShowWarning(ctx context.Context, message string) error {
	log.Println("Send Message:", message)
	params := &ShowMessageParams{
		Type:    Warning,
		Message: message,
	}
	return m.conn.Notify(ctx, "window/showMessage", params)
}

func (m *Messenger) ShowError(ctx context.Context, message string) error {
	log.Println("Send Message:", message)
	params := &ShowMessageParams{
		Type:    Error,
		Message: message,
	}
	return m.conn.Notify(ctx, "window/showMessage", params)
}

// ShowDocument asks the client to open the given document (window/showDocument).
// It is a request, and request handlers in this server run synchronously on the
// read loop, so the call is made from a goroutine to avoid a deadlock.
func (m *Messenger) ShowDocument(uri string, takeFocus bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		params := &ShowDocumentParams{URI: DocumentURI(uri), TakeFocus: &takeFocus}
		var res ShowDocumentResult
		if err := m.conn.Call(ctx, "window/showDocument", params, &res); err != nil {
			log.Println("window/showDocument:", err)
		}
	}()
}

var progressSeq int64

// Progress reports a long-running operation with $/progress. Create it from
// a goroutine (it issues a request and waits for the client). If the client
// does not support work-done progress, all methods are no-ops.
type Progress struct {
	conn  *jsonrpc2.Conn
	token string
	ok    bool
}

func StartProgress(conn *jsonrpc2.Conn, enabled bool, title string) *Progress {
	p := &Progress{conn: conn, token: fmt.Sprintf("sqlh-%d", atomic.AddInt64(&progressSeq, 1))}
	if !enabled || conn == nil {
		return p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Call(ctx, "window/workDoneProgress/create", WorkDoneProgressCreateParams{Token: p.token}, nil); err != nil {
		log.Println("workDoneProgress/create:", err)
		return p
	}
	p.ok = true
	p.notify(WorkDoneProgressValue{Kind: "begin", Title: title})
	return p
}

func (p *Progress) notify(v WorkDoneProgressValue) {
	if !p.ok {
		return
	}
	if err := p.conn.Notify(context.Background(), "$/progress", ProgressParams{Token: p.token, Value: v}); err != nil {
		log.Println("$/progress:", err)
	}
}

func (p *Progress) Report(message string) {
	p.notify(WorkDoneProgressValue{Kind: "report", Message: message})
}

func (p *Progress) End(message string) {
	p.notify(WorkDoneProgressValue{Kind: "end", Message: message})
	p.ok = false
}
