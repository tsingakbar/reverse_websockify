package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/gorilla/websocket"
	"github.com/tsingakbar/reverse_websockify/internal/models/ReverseWebsockify"
	"github.com/tsingakbar/reverse_websockify/internal/models/lockedwebsocket"
)

type configRemoteService struct {
	LocalListenEndpoint string `toml:"local_listen_endpoint"`
}

type config struct {
	HTTPServerEndpoint string                         `toml:"http_server_endpoint"`
	RemoteServices     map[string]configRemoteService `toml:"remote_service"`
}

type clientSideSession struct {
	remoteServiceID string
	connID          uint64
	serviceSideConn *lockedwebsocket.Conn
	clientSideConn  net.Conn
}

func (session *clientSideSession) forwardClientToService() {
	var tcpBuffer [1024]byte
	for {
		n, err := session.clientSideConn.Read(tcpBuffer[0:])
		if err != nil {
			log.Printf("remote service[%s][%d] <= TCP.Read() failed: %s", session.remoteServiceID, session.connID, err)
			return
		}
		fbBuilder := flatbuffers.NewBuilder(0)

		ReverseWebsockify.HeaderStart(fbBuilder)
		ReverseWebsockify.HeaderAddAction(fbBuilder, ReverseWebsockify.ActionForward)
		ReverseWebsockify.HeaderAddConnectionId(fbBuilder, session.connID)
		headerEndAt := ReverseWebsockify.HeaderEnd(fbBuilder)

		fbPayloadEndAt := fbBuilder.CreateByteVector(tcpBuffer[0:n])

		ReverseWebsockify.MessageStart(fbBuilder)
		ReverseWebsockify.MessageAddHeader(fbBuilder, headerEndAt)
		ReverseWebsockify.MessageAddPayload(fbBuilder, fbPayloadEndAt)
		messageEndAt := ReverseWebsockify.MessageEnd(fbBuilder)

		fbBuilder.Finish(messageEndAt)

		if err := session.serviceSideConn.WriteMessage(websocket.BinaryMessage, fbBuilder.FinishedBytes()); err != nil {
			log.Printf("remote service[%s][%d] <= websocket.WriteMessage() failed: %s",
				session.remoteServiceID, session.connID, err)
			return
		}
	}
}

// will terminate forwardingClientToService go routine
func (session *clientSideSession) closeConn() {
	session.clientSideConn.Close()
}

func (session *clientSideSession) forwardServiceToClient(payloadBytes []byte) error {
	if _, err := session.clientSideConn.Write(payloadBytes); err != nil {
		return fmt.Errorf("remote service[%s][%d] => client tcp.Write() failed: %s",
			session.remoteServiceID, session.connID, err)
	}
	return nil
}

type serviceSideSession struct {
	remoteServiceID    string
	since              time.Time
	peerAddrInfo       string
	mtx                sync.Mutex
	connCounter        uint64
	serviceSideConn    *lockedwebsocket.Conn
	clientSideSessions map[uint64]*clientSideSession
}

func (session *serviceSideSession) heartbeatPing() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := session.serviceSideConn.WriteMessage(websocket.PingMessage, []byte{})
			if err != nil {
				log.Printf("remote service[%s] => websocket.WriteMessage(Ping) failed: %s",
					session.remoteServiceID, err)
				return
			}
		}
	}
}

func (session *serviceSideSession) forwardServiceToClients() {
	defer func() {
		// service side websocket is down, close client side connections
		var connIDsToClose []uint64

		session.mtx.Lock()
		for connID := range session.clientSideSessions {
			connIDsToClose = append(connIDsToClose, connID)
		}
		session.mtx.Unlock()

		for _, connID := range connIDsToClose {
			session.closeClientSideConn(connID)
		}

		session.serviceSideConn.Close()
	}()
	for {
		websocketMsgType, websocketMsgBytes, err := session.serviceSideConn.ReadMessage()
		if err != nil {
			log.Printf("remote service[%s] => websocket.ReadMessage() failed: %s", session.remoteServiceID, err)
			return
		}
		if websocketMsgType != websocket.BinaryMessage {
			continue
		}
		fbMsg := ReverseWebsockify.GetRootAsMessage(websocketMsgBytes, 0)
		connID := fbMsg.Header(nil).ConnectionId()

		session.mtx.Lock()
		clientSideSession, ok := session.clientSideSessions[connID]
		session.mtx.Unlock()

		if !ok {
			session.notifyServiceSide(ReverseWebsockify.ActionClose, connID)
			continue
		}

		var needCloseClientSide = false
		switch fbMsg.Header(nil).Action() {
		case ReverseWebsockify.ActionEstablished:
			go session.handleRemoteServiceSideEstablishedConnectionForClient(clientSideSession, connID)
		case ReverseWebsockify.ActionForward:
			if err := clientSideSession.forwardServiceToClient(fbMsg.PayloadBytes()); err != nil {
				log.Println(err.Error())
				needCloseClientSide = true
			}
		case ReverseWebsockify.ActionClose:
			needCloseClientSide = true
		}
		if needCloseClientSide {
			session.closeClientSideConn(connID)
		}
	}
}

func (session *serviceSideSession) notifyServiceSide(action ReverseWebsockify.Action, connID uint64) {
	fbBuilder := flatbuffers.NewBuilder(0)

	ReverseWebsockify.HeaderStart(fbBuilder)
	ReverseWebsockify.HeaderAddAction(fbBuilder, action)
	ReverseWebsockify.HeaderAddConnectionId(fbBuilder, connID)
	headerEndAt := ReverseWebsockify.HeaderEnd(fbBuilder)

	ReverseWebsockify.MessageStart(fbBuilder)
	ReverseWebsockify.MessageAddHeader(fbBuilder, headerEndAt)
	messageEndAt := ReverseWebsockify.MessageEnd(fbBuilder)

	fbBuilder.Finish(messageEndAt)

	if err := session.serviceSideConn.WriteMessage(websocket.BinaryMessage, fbBuilder.FinishedBytes()); err != nil {
		log.Printf("%s[%d]: websocket.WriteMessage() failed: %s", session.remoteServiceID, connID, err)
		return
	}
}

func (session *serviceSideSession) handleClientSideNewConn(connTCP net.Conn) {
	session.mtx.Lock()
	connID := session.connCounter
	session.connCounter++
	clientSideSession := &clientSideSession{
		remoteServiceID: session.remoteServiceID,
		connID:          connID,
		serviceSideConn: session.serviceSideConn,
		clientSideConn:  connTCP,
	}
	session.clientSideSessions[connID] = clientSideSession
	session.mtx.Unlock()

	log.Printf("remote service[%s][%d] <= accepted new client side connection, will notify remote service side",
		session.remoteServiceID, connID)
	session.notifyServiceSide(ReverseWebsockify.ActionConnect, connID)
	// NOTE: only after remote service side notified us the connection is established,
	//       then we can forward client side message to remote service side.
}

func (session *serviceSideSession) handleRemoteServiceSideEstablishedConnectionForClient(
	clientSideSession *clientSideSession, connID uint64) {
	log.Printf("remote service[%s][%d] => established upper connection, will forward client side data to it",
		session.remoteServiceID, connID)
	clientSideSession.forwardClientToService()
	session.closeClientSideConn(connID)
}

func (session *serviceSideSession) closeClientSideConn(connID uint64) {
	session.mtx.Lock()
	clientSideSession, ok := session.clientSideSessions[connID]
	if ok {
		delete(session.clientSideSessions, connID)
	}
	session.mtx.Unlock()

	if ok {
		clientSideSession.closeConn()
		log.Printf("remote service[%s][%d] <= close client side connection, will notfiy remote sevice side",
			session.remoteServiceID, connID)
		session.notifyServiceSide(ReverseWebsockify.ActionClose, connID)
	}
}

type reverseWebsockifyForwarder struct {
	mtx                  sync.Mutex
	confRemoteServices   map[string]configRemoteService
	serviceSideSesssions map[string]*serviceSideSession
}

func (forwarder *reverseWebsockifyForwarder) onNewClientSideConnection(remoteServiceID string, conn net.Conn) error {
	forwarder.mtx.Lock()
	serviceSideSession, ok := forwarder.serviceSideSesssions[remoteServiceID]
	forwarder.mtx.Unlock()

	if !ok {
		return fmt.Errorf("No active remote service websocket session for forwarding")
	}
	go serviceSideSession.handleClientSideNewConn(conn)
	return nil
}

func (forwarder *reverseWebsockifyForwarder) listenLocalEndpoints() {
	for remoteServiceID, remoteServiceConf := range forwarder.confRemoteServices {
		go func(remoteServiceID, listenEndpoint string) {
			log.Printf("remote service[%s] <= listen %s", remoteServiceID, listenEndpoint)
			netListener, err := net.Listen("tcp", listenEndpoint)
			if err != nil {
				log.Panicf("remote service[%s] error listening %s: %s", remoteServiceID, listenEndpoint, err.Error())
			}
			for {
				clientConn, err := netListener.Accept()
				if err != nil {
					log.Printf("remote service[%s] error accepting client connection from %s: %s",
						remoteServiceID, listenEndpoint, err.Error())
					continue
				}
				if err = forwarder.onNewClientSideConnection(remoteServiceID, clientConn); err != nil {
					log.Printf("remote service[%s] error handling new client side connection from %s: %s",
						remoteServiceID, listenEndpoint, err.Error())
					clientConn.Close()
				}
			}
		}(remoteServiceID, remoteServiceConf.LocalListenEndpoint)
	}
}

func (forwarder *reverseWebsockifyForwarder) onNewServiceSideConnection(
	remoteServiceID string, conn *lockedwebsocket.Conn, peerAddrInfo string) {
	newServiceSideSession := &serviceSideSession{
		remoteServiceID:    remoteServiceID,
		since:              time.Now(),
		peerAddrInfo:       peerAddrInfo,
		serviceSideConn:    conn,
		clientSideSessions: make(map[uint64]*clientSideSession),
	}

	forwarder.mtx.Lock()
	// NOTE: there might exists old serviceSideSession, just let it drift away
	forwarder.serviceSideSesssions[remoteServiceID] = newServiceSideSession
	forwarder.mtx.Unlock()
	log.Printf("remote service[%s] => agent connected via websocket", remoteServiceID)

	go newServiceSideSession.heartbeatPing()
	newServiceSideSession.forwardServiceToClients()

	forwarder.mtx.Lock()
	delete(forwarder.serviceSideSesssions, remoteServiceID)
	forwarder.mtx.Unlock()
}

func (forwarder *reverseWebsockifyForwarder) serviceSideWebsocketHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		Subprotocols: websocket.Subprotocols(r),
	}
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("failed to upgrade %s connection to websocket: %s", r.URL.Path, err.Error())
		return
	}
	peerAddrInfo := r.Header.Get("X-Real-Ip")
	if len(peerAddrInfo) == 0 {
		peerAddrInfo = wsConn.RemoteAddr().String()
	}
	go forwarder.onNewServiceSideConnection(r.URL.Path, lockedwebsocket.CreateConn(wsConn), peerAddrInfo)
}

func (forwarder *reverseWebsockifyForwarder) statHandler(w http.ResponseWriter, _ *http.Request) {
	type statType struct {
		ForwardAt            string    `json:"forward_at"`
		ServiceSideConnSince time.Time `json:"service_connection_since"`
		ServiceSideConnInfo  string    `json:"service_connection_info"`
		ClientSideConnCount  int       `json:"client_connection_count"`
	}
	var stats = make(map[string]statType)

	for serviceID, confRemoteService := range forwarder.confRemoteServices {
		var stat = statType{
			ForwardAt: confRemoteService.LocalListenEndpoint,
		}

		forwarder.mtx.Lock()
		if serviceSideSession, ok := forwarder.serviceSideSesssions[serviceID]; ok {
			stat.ServiceSideConnSince = serviceSideSession.since
			stat.ServiceSideConnInfo = serviceSideSession.peerAddrInfo
			serviceSideSession.mtx.Lock()
			stat.ClientSideConnCount = len(serviceSideSession.clientSideSessions)
			serviceSideSession.mtx.Unlock()
		}
		forwarder.mtx.Unlock()

		stats[serviceID] = stat
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "./exe path/to/config.toml")
		return
	}
	var conf config
	if _, err := toml.DecodeFile(os.Args[1], &conf); err != nil {
		log.Fatal(err)
	}
	var forwarder = reverseWebsockifyForwarder{
		confRemoteServices:   conf.RemoteServices,
		serviceSideSesssions: make(map[string]*serviceSideSession),
	}
	go forwarder.listenLocalEndpoints()
	http.Handle("/forward_to_me/",
		http.StripPrefix("/forward_to_me/", http.HandlerFunc(forwarder.serviceSideWebsocketHandler)))
	http.HandleFunc("/", forwarder.statHandler)
	log.Printf("start http server on %s", conf.HTTPServerEndpoint)
	if err := http.ListenAndServe(conf.HTTPServerEndpoint, nil); err != nil {
		log.Printf("http.ListenAndServe returns error: %s", err.Error())
	}
}
