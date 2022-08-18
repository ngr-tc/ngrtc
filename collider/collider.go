// Copyright (c) 2014 The WebRTC project authors. All Rights Reserved.
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file in the root of the source
// tree.

// Package collider implements a signaling server based on WebSocket.
package collider

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/websocket"
	"html"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const registerTimeoutSec = 60 * 10

// This is a temporary solution to avoid holding a zombie connection forever, by
// setting a 1 day timeout on reading from the WebSocket connection.
const wsReadTimeoutSec = 60 * 60 * 24

const RESPONSE_ERROR = "ERROR"
const RESPONSE_ROOM_FULL = "FULL"
const RESPONSE_UNKNOWN_ROOM = "UNKNOWN_ROOM"
const RESPONSE_UNKNOWN_CLIENT = "UNKNOWN_CLIENT"
const RESPONSE_DUPLICATE_CLIENT = "DUPLICATE_CLIENT"
const RESPONSE_SUCCESS = "SUCCESS"
const RESPONSE_INVALID_REQUEST = "INVALID_REQUEST"

const LOOPBACK_CLIENT_ID = "LOOPBACK_CLIENT_ID"

const runesDigital = "0123456789"

type Response struct {
	Result string                 `json:"result"`
	Params map[string]interface{} `json:"params"`
}

// ICEServer describes a single STUN and TURN server that can be used by
// the ICEAgent to establish a connection with a peer.
type ICEServer struct {
	URLs       []string    `json:"urls"`
	Username   string      `json:"username,omitempty"`
	Credential interface{} `json:"credential,omitempty"`
}

func generateRandom(n int, runes string) string {
	letters := []rune(runes)
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

type Collider struct {
	*roomTable
	dash *dashboard
}

func NewCollider(rs string) *Collider {
	return &Collider{
		roomTable: newRoomTable(time.Second*registerTimeoutSec, rs),
		dash:      newDashboard(),
	}
}

// Run starts the collider server and blocks the thread until the program exits.
func (c *Collider) Run(p int, useTls bool) {
	http.Handle("/ws", websocket.Handler(c.wsHandler))
	http.HandleFunc("/status", c.httpStatusHandler)
	http.HandleFunc("/", c.httpHandler)

	var e error

	pstr := ":" + strconv.Itoa(p)
	if useTls {
		config := &tls.Config{
			// Only allow ciphers that support forward secrecy for iOS9 compatibility:
			// https://developer.apple.com/library/prerelease/ios/technotes/App-Transport-Security-Technote/
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			},
			PreferServerCipherSuites: true,
		}
		server := &http.Server{Addr: pstr, Handler: nil, TLSConfig: config}

		e = server.ListenAndServeTLS("/cert/cert.pem", "/cert/key.pem")
	} else {
		e = http.ListenAndServe(pstr, nil)
	}

	if e != nil {
		log.Fatal("Run: " + e.Error())
	}
}

func (c *Collider) addClientToRoom(room_id, client_id string, is_loopback bool) (is_initiator bool, messages []string, err error) {
	room := c.roomTable.room(room_id)
	occupancy := len(room.clients)
	if occupancy >= 2 {
		err = errors.New(RESPONSE_ROOM_FULL)
	} else if _, ok := room.clients[client_id]; ok {
		err = errors.New(RESPONSE_DUPLICATE_CLIENT)
	} else if occupancy == 0 {
		is_initiator = true
		room.client(client_id)
		if is_loopback {
			room.client(LOOPBACK_CLIENT_ID)
		}
	} else {
		is_initiator = false
		other_client, _ := room.other_client(client_id)
		messages = other_client.msgs
		room.client(client_id)
		other_client.msgs = nil
	}
	return
}

func (c *Collider) removeClientFromRoom(room_id, client_id string) error {
	if _, ok := c.roomTable.rooms[room_id]; !ok {
		fmt.Printf("Unknown room: %s\n", room_id)
		return errors.New(RESPONSE_UNKNOWN_ROOM)
	}
	room := c.roomTable.room(room_id)
	if _, ok := room.clients[client_id]; !ok {
		fmt.Printf("Unknown client: %s\n", client_id)
		return errors.New(RESPONSE_UNKNOWN_CLIENT)
	}
	room.remove(client_id)
	if _, ok := room.clients[LOOPBACK_CLIENT_ID]; ok {
		room.remove(LOOPBACK_CLIENT_ID)
	}
	if len(room.clients) > 0 {
		if client, err := room.other_client(client_id); err == nil {
			client.is_initiator = true
		}
	} else {
		delete(c.roomTable.rooms, room_id)
	}
	return nil
}

func (c *Collider) saveMessageFromClient(room_id, client_id string, message string) (saved bool, err error) {
	if _, ok := c.roomTable.rooms[room_id]; !ok {
		fmt.Printf("Unknown room: %s\n", room_id)
		err = errors.New(RESPONSE_UNKNOWN_ROOM)
		return
	}
	room := c.roomTable.room(room_id)
	client, ok := room.clients[client_id]
	if !ok {
		fmt.Printf("Unknown client: %s\n", client_id)
		err = errors.New(RESPONSE_UNKNOWN_CLIENT)
		return
	}
	if len(room.clients) > 1 {
		return
	}

	client.enqueue(message)
	saved = true
	fmt.Printf("Saved message for room  %s client %s with message %s, total saved msg count %d\n", room_id, client_id, message, len(client.msgs))

	return
}

func (c *Collider) sendMessageToCollider(w http.ResponseWriter, room_id, client_id string, message string) {
	fmt.Printf("Forwarding message to collider for room  %s client %s with message %s\n", room_id, client_id, message)
	if err := c.roomTable.send(room_id, client_id, message); err != nil {
		c.httpError("Failed to send the message: "+err.Error(), w)
		return
	}
	c.writeMessageResponse(w, RESPONSE_SUCCESS)
}

// Returns appropriate room parameters based on query parameters in the request.
func (c *Collider) getRoomParameters(room_id, client_id string, is_initiator bool) map[string]interface{} {
	params := make(map[string]interface{})
	params["room_id"] = room_id
	params["client_id"] = client_id
	params["is_initiator"] = is_initiator
	params["wss_url"] = "ws://" + c.roomSrvUrl + ":443/ws"
	params["wss_post_url"] = "http://" + c.roomSrvUrl + ":443"
	return params
}

func (c *Collider) writeJoinResponse(w http.ResponseWriter, result string, params map[string]interface{}, messages []string) {
	if messages != nil {
		params["messages"] = messages
	}
	rp := Response{
		Result: result,
		Params: params,
	}

	enc := json.NewEncoder(w)
	if err := enc.Encode(rp); err != nil {
		err = errors.New("Failed to encode to JSON: err=" + err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (c *Collider) writeMessageResponse(w http.ResponseWriter, result string) {
	rp := Response{
		Result: result,
	}

	enc := json.NewEncoder(w)
	if err := enc.Encode(rp); err != nil {
		err = errors.New("Failed to encode to JSON: err=" + err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// httpStatusHandler is a HTTP handler that handles GET requests to get the
// status of collider.
func (c *Collider) httpStatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Allow-Methods", "GET")

	rp := c.dash.getReport(c.roomTable)
	enc := json.NewEncoder(w)
	if err := enc.Encode(rp); err != nil {
		err = errors.New("Failed to encode to JSON: err=" + err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		c.dash.onHttpErr(err)
	}
}

// httpJoinHandler is a HTTP handler that handles Post requests to join the room
// A POST /join/<room id>
func (c *Collider) httpJoinHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("httpJoinHandler: %v\n", r)
	p := strings.Split(r.URL.Path, "/")
	if len(p) != 3 {
		c.httpError("Invalid path: "+html.EscapeString(r.URL.Path), w)
		return
	}
	var room_id string
	is_loopback := false
	if strings.Contains(p[2], "debug=loopback") {
		is_loopback = true
		pp := strings.Split(p[2], "?")
		room_id = pp[0]
	} else {
		room_id = p[2]
	}
	client_id := generateRandom(8, runesDigital)
	is_initiator, messages, err := c.addClientToRoom(room_id, client_id, is_loopback)
	if err != nil {
		fmt.Printf("Error adding client to room: %v\n", err)
		c.writeJoinResponse(w, err.Error(), make(map[string]interface{}), nil)
		return
	}

	fmt.Printf("User %s joined room %s\n", client_id, room_id)
	params := c.getRoomParameters(room_id, client_id, is_initiator)
	c.writeJoinResponse(w, "SUCCESS", params, messages)
}

// httpParamsHandler is a HTTP handler that handles Post requests to get Params
// A GET /params
func (c *Collider) httpParamsHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("httpParamsHandler: %v\n", r)
	params := make(map[string]interface{})
	iceServers := []ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	}
	params["iceServers"] = iceServers

	enc := json.NewEncoder(w)
	if err := enc.Encode(params); err != nil {
		err = errors.New("Failed to encode to JSON: err=" + err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// httpMessageHandler is a HTTP handler that handles Post requests to message the room
// A POST /message/<room id>/<client id> offer
// A POST /message/<room id>/<client id> candidate
func (c *Collider) httpMessageHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("httpMessageHandler: %v\n", r)
	p := strings.Split(r.URL.Path, "/")
	if len(p) != 4 {
		c.httpError("Invalid path: "+html.EscapeString(r.URL.Path), w)
		return
	}
	room_id := p[2]
	client_id := p[3]

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		c.httpError("Failed to read request body: "+err.Error(), w)
		return
	}
	message := string(body)
	if message == "" {
		c.httpError("Empty request body", w)
		return
	}

	saved, err := c.saveMessageFromClient(room_id, client_id, message)
	if err != nil {
		c.writeMessageResponse(w, err.Error())
	}
	if !saved {
		// Other client joined, forward to collider. Do this outside the lock.
		// Note: this may fail in local dev server due to not having the right
		// certificate file locally for SSL validation.
		// Note: loopback scenario follows this code path.
		c.sendMessageToCollider(w, room_id, client_id, message)
	} else {
		c.writeMessageResponse(w, RESPONSE_SUCCESS)
	}
}

// httpLeaveHandler is a HTTP handler that handles Post requests to leave the room
// A POST /leave/<room id>/<client id>
func (c *Collider) httpLeaveHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("httpLeaveHandler: %v\n", r)
	p := strings.Split(r.URL.Path, "/")
	if len(p) != 4 {
		c.httpError("Invalid path: "+html.EscapeString(r.URL.Path), w)
		return
	}
	room_id := p[2]
	client_id := p[3]
	if err := c.removeClientFromRoom(room_id, client_id); err != nil {
		fmt.Printf("removeClientFromRoom %s get error %s\n", room_id, err)
	}
}

// httpHandler is a HTTP handler that handles GET/POST/DELETE requests.
// POST request to path "/$ROOMID/$CLIENTID" is used to send a message to the other client of the room.
// $CLIENTID is the source client ID.
// The request must have a form value "msg", which is the message to send.
// DELETE request to path "/$ROOMID/$CLIENTID" is used to delete all records of a client, including the queued message from the client.
// "OK" is returned if the request is valid.
func (c *Collider) httpHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Allow-Methods", "POST, DELETE")

	/*
		/join/<room id>
		/leave/<room id>/<client id>
		/message/<room id>/<client id> offer
		/message/<room id>/<client id> candidate
	*/
	p := strings.Split(r.URL.Path, "/")
	if len(p) < 2 {
		c.httpError("Invalid path: "+html.EscapeString(r.URL.Path), w)
		return
	}
	if p[1] == "join" {
		c.httpJoinHandler(w, r)
		return
	} else if p[1] == "params" {
		c.httpParamsHandler(w, r)
		return
	} else if p[1] == "leave" {
		c.httpLeaveHandler(w, r)
		return
	} else if p[1] == "message" {
		c.httpMessageHandler(w, r)
		return
	}

	// otherwise
	fmt.Printf("httpHandler: %v\n", r)

	if len(p) != 3 {
		c.httpError("Invalid path: "+html.EscapeString(r.URL.Path), w)
		return
	}
	rid, cid := p[1], p[2]

	switch r.Method {
	case "POST":
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			c.httpError("Failed to read request body: "+err.Error(), w)
			return
		}
		m := string(body)
		if m == "" {
			c.httpError("Empty request body", w)
			return
		}
		if err := c.roomTable.send(rid, cid, m); err != nil {
			c.httpError("Failed to send the message: "+err.Error(), w)
			return
		}
	case "DELETE":
		c.roomTable.remove(rid, cid)
	default:
		return
	}

	io.WriteString(w, "OK\n")
}

// wsHandler is a WebSocket server that handles requests from the WebSocket client in the form of:
// 1. { 'cmd': 'register', 'roomid': $ROOM, 'clientid': $CLIENT' },
// which binds the WebSocket client to a client ID and room ID.
// A client should send this message only once right after the connection is open.
// or
// 2. { 'cmd': 'send', 'msg': $MSG }, which sends the message to the other client of the room.
// It should be sent to the server only after 'regiser' has been sent.
// The message may be cached by the server if the other client has not joined.
//
// Unexpected messages will cause the WebSocket connection to be closed.
func (c *Collider) wsHandler(ws *websocket.Conn) {
	var rid, cid string

	registered := false

	var msg wsClientMsg
loop:
	for {
		err := ws.SetReadDeadline(time.Now().Add(time.Duration(wsReadTimeoutSec) * time.Second))
		if err != nil {
			c.wsError("ws.SetReadDeadline error: "+err.Error(), ws)
			break
		}

		err = websocket.JSON.Receive(ws, &msg)
		if err != nil {
			if err.Error() != "EOF" {
				c.wsError("websocket.JSON.Receive error: "+err.Error(), ws)
			}
			break
		}

		log.Printf("WebSocket received %s from room %s client %s\n", msg.Cmd, msg.RoomID, msg.ClientID)

		switch msg.Cmd {
		case "register":
			if registered {
				c.wsError("Duplicated register request", ws)
				break loop
			}
			if msg.RoomID == "" || msg.ClientID == "" {
				c.wsError("Invalid register request: missing 'clientid' or 'roomid'", ws)
				break loop
			}
			if err = c.roomTable.register(msg.RoomID, msg.ClientID, ws); err != nil {
				c.wsError(err.Error(), ws)
				break loop
			}
			registered, rid, cid = true, msg.RoomID, msg.ClientID
			c.dash.incrWs()

			defer c.roomTable.deregister(rid, cid)
			break
		case "send":
			if !registered {
				c.wsError("Client not registered", ws)
				break loop
			}
			if msg.Msg == "" {
				c.wsError("Invalid send request: missing 'msg'", ws)
				break loop
			}
			c.roomTable.send(rid, cid, msg.Msg)
			break
		default:
			c.wsError("Invalid message: unexpected 'cmd'", ws)
			break
		}
	}
	// This should be unnecessary but just be safe.
	ws.Close()
}

func (c *Collider) httpError(msg string, w http.ResponseWriter) {
	err := errors.New(msg)
	http.Error(w, err.Error(), http.StatusInternalServerError)
	c.dash.onHttpErr(err)
}

func (c *Collider) wsError(msg string, ws *websocket.Conn) {
	err := errors.New(msg)
	sendServerErr(ws, msg)
	c.dash.onWsErr(err)
}
