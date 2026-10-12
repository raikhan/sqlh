package lsp

import (
	"context"
	"log"
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
