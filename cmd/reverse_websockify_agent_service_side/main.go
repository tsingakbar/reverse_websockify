package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/gorilla/websocket"
	"github.com/tsingakbar/reverse_websockify/internal/models/ReverseWebsockify"
	"github.com/tsingakbar/reverse_websockify/internal/models/lockedwebsocket"
)

var (
	localServiceAddr *string
	websockifyAddr   *string
	clientCertPath   *string
	clientKeyPath    *string
	caCertPath       *string
)

func init() {
	localServiceAddr = flag.String("service", "127.0.0.1:80", "tcp service endpoint to forward to")
	websockifyAddr = flag.String(
		"reverse-websockify", "ws://127.0.0.1:39000/forward_to_me/LKLAFA151NA774655", "reverse websockify endpoint")
	clientCertPath = flag.String("client-cert", "client.cert.pem", "")
	clientKeyPath = flag.String("client-key", "client.key.pem", "")
	caCertPath = flag.String("ca-cert", "ca.cert.pem", "")
}

func loadTLSConfig(certFile, keyFile, caFile string) *tls.Config {
	// Load client certificate
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("error loading client certificates: %v", err)
	}

	// Load CA cert
	caCert, err := os.ReadFile(caFile)
	if err != nil {
		log.Fatalf("error loading CA certificate: %v", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		RootCAs:            caCertPool,
		ClientCAs:          caCertPool,
		ClientAuth:         tls.RequireAndVerifyClientCert,
		InsecureSkipVerify: true,
	}
}

type websockifyClient struct {
	websockifyConn    *lockedwebsocket.Conn
	mtx               sync.Mutex
	localServiceConns map[uint64]net.Conn
}

func (client *websockifyClient) connectWebsockifyAndServe() {
	u, err := url.Parse(*websockifyAddr)
	if err != nil || !(u.Scheme == "ws" || u.Scheme == "wss") {
		log.Fatalf("%s is not a valid websockify url endpoint", *websockifyAddr)
	}
	log.Printf("%s connecting...", *websockifyAddr)
	dialer := websocket.DefaultDialer
	if u.Scheme == "wss" {
		dialer = &websocket.Dialer{
			TLSClientConfig: loadTLSConfig(*clientCertPath, *clientKeyPath, *caCertPath),
		}
	}
	wsConn, _, err := dialer.Dial(*websockifyAddr, nil)
	if err != nil {
		log.Printf("Error connecting to WebSocket server: %s", err.Error())
		return
	}
	defer wsConn.Close()
	client.websockifyConn = lockedwebsocket.CreateConn(wsConn)
	client.dispatchIncomeWebsockifyMsg()
}

func (client *websockifyClient) dispatchIncomeWebsockifyMsg() {
	log.Printf("dispatching incoming websocket msgs...")
	for {
		websocketMsgType, websocketMsgBytes, err := client.websockifyConn.ReadMessage()
		if err != nil {
			log.Printf("websocket.ReadMessage() failed: %s", err)
			return
		}
		if websocketMsgType != websocket.BinaryMessage {
			continue
		}
		fbMsg := ReverseWebsockify.GetRootAsMessage(websocketMsgBytes, 0)
		switch fbMsg.Header(nil).Action() {
		case ReverseWebsockify.ActionConnect:
			go client.forwardLocalServiceToWebsockify(fbMsg.Header(nil).ConnectionId())
		case ReverseWebsockify.ActionClose:
			client.closeLocalServiceConn(fbMsg.Header(nil).ConnectionId())
		case ReverseWebsockify.ActionForward:
			// TODO(richieyu) avoid write blocking
			if err := client.forwardWebsockifyPlayloadToLocalService(fbMsg); err != nil {
				client.closeLocalServiceConn(fbMsg.Header(nil).ConnectionId())
			}
		}
	}
}

func (client *websockifyClient) forwardLocalServiceToWebsockify(connID uint64) {
	defer client.notifyWebsockifySide(ReverseWebsockify.ActionClose, connID)

	log.Printf("local service connection[%d] connecting to %s...", connID, *localServiceAddr)
	connLocalService, err := net.Dial("tcp", *localServiceAddr)
	if err != nil {
		log.Printf("local service connection[%d] failed to create: %s", connID, err)
		return
	}

	client.mtx.Lock()
	client.localServiceConns[connID] = connLocalService
	client.mtx.Unlock()

	log.Printf("local service connection[%d] created, will notify websockify", connID)
	client.notifyWebsockifySide(ReverseWebsockify.ActionEstablished, connID)

	defer client.closeLocalServiceConn(connID)

	var tcpBuffer [1024]byte
	for {
		n, err := connLocalService.Read(tcpBuffer[0:])
		if err != nil {
			log.Printf("local service connection[%d] TCP.Read() failed: %s", connID, err.Error())
			return
		}

		fbBuilder := flatbuffers.NewBuilder(0)

		ReverseWebsockify.HeaderStart(fbBuilder)
		ReverseWebsockify.HeaderAddAction(fbBuilder, ReverseWebsockify.ActionForward)
		ReverseWebsockify.HeaderAddConnectionId(fbBuilder, connID)
		headerEndAt := ReverseWebsockify.HeaderEnd(fbBuilder)

		fbPayloadEndAt := fbBuilder.CreateByteVector(tcpBuffer[0:n])

		ReverseWebsockify.MessageStart(fbBuilder)
		ReverseWebsockify.MessageAddHeader(fbBuilder, headerEndAt)
		ReverseWebsockify.MessageAddPayload(fbBuilder, fbPayloadEndAt)
		messageEndAt := ReverseWebsockify.MessageEnd(fbBuilder)

		fbBuilder.Finish(messageEndAt)

		if err := client.websockifyConn.WriteMessage(websocket.BinaryMessage, fbBuilder.FinishedBytes()); err != nil {
			log.Printf("local service connection[%d] => websocket.WriteMessage() failed: %s", connID, err)
			return
		}
	}
}

func (client *websockifyClient) closeLocalServiceConn(connID uint64) {
	client.mtx.Lock()
	connLocalService, ok := client.localServiceConns[connID]
	if ok {
		delete(client.localServiceConns, connID)
	}
	client.mtx.Unlock()

	if ok {
		connLocalService.Close()
		log.Printf("local service connection[%d] closed", connID)
	}
}

func (client *websockifyClient) forwardWebsockifyPlayloadToLocalService(fbMsg *ReverseWebsockify.Message) error {
	connID := fbMsg.Header(nil).ConnectionId()
	client.mtx.Lock()
	localServiceConn, ok := client.localServiceConns[connID]
	client.mtx.Unlock()

	if !ok {
		client.notifyWebsockifySide(ReverseWebsockify.ActionClose, fbMsg.Header(nil).ConnectionId())
		return nil
	}
	if _, err := localServiceConn.Write(fbMsg.PayloadBytes()); err != nil {
		return fmt.Errorf("local service connection[%d] <= TCP.Write() failed: %s", connID, err)
	}
	return nil
}

func (client *websockifyClient) notifyWebsockifySide(action ReverseWebsockify.Action, connID uint64) {
	fbBuilder := flatbuffers.NewBuilder(0)

	ReverseWebsockify.HeaderStart(fbBuilder)
	ReverseWebsockify.HeaderAddAction(fbBuilder, action)
	ReverseWebsockify.HeaderAddConnectionId(fbBuilder, connID)
	headerEndAt := ReverseWebsockify.HeaderEnd(fbBuilder)

	ReverseWebsockify.MessageStart(fbBuilder)
	ReverseWebsockify.MessageAddHeader(fbBuilder, headerEndAt)
	messageEndAt := ReverseWebsockify.MessageEnd(fbBuilder)

	fbBuilder.Finish(messageEndAt)

	if err := client.websockifyConn.WriteMessage(websocket.BinaryMessage, fbBuilder.FinishedBytes()); err != nil {
		log.Printf("local service connection[%d] => websocket.WriteMessage() failed: %s", connID, err)
		return
	}
}

func main() {
	flag.Parse()

	for {
		var websockifyClient = websockifyClient{
			localServiceConns: make(map[uint64]net.Conn),
		}
		websockifyClient.connectWebsockifyAndServe()
		time.Sleep(10 * time.Second)
	}
}
