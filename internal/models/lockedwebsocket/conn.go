package lockedwebsocket

import (
	"sync"

	"github.com/gorilla/websocket"
)

// Conn wrap around websocket.Conn to and a lock and counters
// see also: https://pkg.go.dev/github.com/gorilla/websocket#hdr-Concurrency
type Conn struct {
	conn         *websocket.Conn
	mtxWrite     sync.Mutex
	BytesWritten uint64
	mtxRead      sync.Mutex
	BytesRead    uint64
}

// CreateConn factory function
func CreateConn(wsConn *websocket.Conn) *Conn {
	return &Conn{
		conn: wsConn,
	}
}

// WriteMessage wrap around originanl WriteMessage
func (ws *Conn) WriteMessage(messageType int, data []byte) error {
	ws.mtxWrite.Lock()
	defer ws.mtxWrite.Unlock()
	ws.BytesWritten += uint64(len(data))
	return ws.conn.WriteMessage(messageType, data)
}

// ReadMessage wrap around originanl ReadMessage
func (ws *Conn) ReadMessage() (int, []byte, error) {
	ws.mtxRead.Lock()
	defer ws.mtxRead.Unlock()
	messageType, data, err := ws.conn.ReadMessage()
	ws.BytesRead += uint64(len(data))
	return messageType, data, err
}

// Close wrap around Close
func (ws *Conn) Close() error {
	return ws.conn.Close()
}
