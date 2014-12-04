package gumble

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"time"

	"code.google.com/p/goprotobuf/proto"
	"github.com/bontibon/gopus"
	"github.com/bontibon/gumble/gumble/MumbleProto"
)

// State is the current state of the client's connection to the server.
type State int

const (
	// The client is current disconnected from the server.
	StateDisconnected State = iota

	// The client is connected to the server, but has yet to receive the current
	// server state.
	StateConnected

	// The client is connected to the server and has been sent the server state.
	StateSynced
)

// Request is a mask of items that the client can ask the server to send.
type Request int

const (
	RequestDescription Request = 1 << iota
	RequestComment
	RequestTexture
	RequestStats
	RequestUserList
	RequestAcl
	RequestBanList
)

// PingInterval is the interval at which ping packets are be sent by the client
// to the server.
const pingInterval time.Duration = time.Second * 10

// maximumPacketSize is the maximum length in bytes of a packet that will be
// accepted from the server.
const maximumPacketSize = 1024 * 1024 * 10 // 10 megabytes

var (
	ErrState = errors.New("client is in an invalid state")
)

type Client struct {
	config *Config

	state  State
	self   *User
	server struct {
		version Version
	}

	connection *tls.Conn
	tls        tls.Config

	users          Users
	channels       Channels
	contextActions ContextActions

	audioEncoder  *gopus.Encoder
	audioSequence int
	audioTarget   *VoiceTarget

	end        chan bool
	closeMutex sync.Mutex
	sendMutex  sync.Mutex
}

// NewClient creates a new gumble client. Returns nil if config is nil.
func NewClient(config *Config) *Client {
	if config == nil {
		return nil
	}
	client := &Client{
		config: config,
		state:  StateDisconnected,
	}
	return client
}

// Connect connects to the server.
func (c *Client) Connect() error {
	if c.state != StateDisconnected {
		return ErrState
	}
	if encoder, err := gopus.NewEncoder(AudioSampleRate, 1, gopus.Voip); err != nil {
		return err
	} else {
		encoder.SetVbr(false)
		c.audioEncoder = encoder
		c.audioSequence = 0
		c.audioTarget = nil
	}
	if conn, err := tls.DialWithDialer(&c.config.Dialer, "tcp", c.config.Address, &c.config.TlsConfig); err != nil {
		c.audioEncoder = nil
		return err
	} else {
		c.connection = conn
	}
	c.users = Users{}
	c.channels = Channels{}
	c.contextActions = ContextActions{}
	c.state = StateConnected

	// Channels and goroutines
	c.end = make(chan bool)
	go c.readRoutine()
	go c.pingRoutine()

	// Initial packets
	version := Version{
		release:   "gumble",
		os:        runtime.GOOS,
		osVersion: runtime.GOARCH,
	}
	version.setSemanticVersion(1, 2, 4)

	versionPacket := MumbleProto.Version{
		Version:   &version.version,
		Release:   &version.release,
		Os:        &version.os,
		OsVersion: &version.osVersion,
	}
	authenticationPacket := MumbleProto.Authenticate{
		Username: &c.config.Username,
		Password: &c.config.Password,
		Opus:     proto.Bool(true),
		Tokens:   c.config.Tokens,
	}
	c.Send(protoMessage{&versionPacket})
	c.Send(protoMessage{&authenticationPacket})
	return nil
}

// pingRoutine sends ping packets to the server at regular intervals.
func (c *Client) pingRoutine() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	pingPacket := MumbleProto.Ping{
		Timestamp: proto.Uint64(0),
	}
	pingProto := protoMessage{&pingPacket}

	for {
		select {
		case <-c.end:
			return
		case time := <-ticker.C:
			*pingPacket.Timestamp = uint64(time.Unix())
			c.Send(pingProto)
		}
	}
}

// readRoutine reads protocol buffer messages from the server.
func (c *Client) readRoutine() {
	defer c.close(&DisconnectEvent{
		Client: c,
		Type:   DisconnectError,
	})

	conn := c.connection
	data := make([]byte, 1024)

	for {
		var pType uint16
		var pLength uint32

		conn.SetReadDeadline(time.Now().Add(pingInterval * 2))
		if err := binary.Read(conn, binary.BigEndian, &pType); err != nil {
			return
		}
		if err := binary.Read(conn, binary.BigEndian, &pLength); err != nil {
			return
		}
		pLengthInt := int(pLength)
		if pLengthInt > maximumPacketSize {
			return
		}
		if pLengthInt > cap(data) {
			data = make([]byte, pLengthInt)
		}
		if _, err := io.ReadFull(conn, data[:pLengthInt]); err != nil {
			return
		}
		if handle, ok := handlers[pType]; ok {
			handle(c, data[:pLengthInt])
		}
	}
}

// AudioEncoder returns the audio encoder used when sending audio to the
// server.
func (c *Client) AudioEncoder() *gopus.Encoder {
	return c.audioEncoder
}

// Request requests that specific server information be sent to the client. The
// supported request types are: RequestUserList, and RequestBanList.
func (c *Client) Request(request Request) {
	if (request & RequestUserList) != 0 {
		packet := MumbleProto.UserList{}
		proto := protoMessage{&packet}
		c.Send(proto)
	}
	if (request & RequestBanList) != 0 {
		packet := MumbleProto.BanList{
			Query: proto.Bool(true),
		}
		proto := protoMessage{&packet}
		c.Send(proto)
	}
}

// Disconnect disconnects the client from the server.
func (c *Client) Disconnect() error {
	return c.close(&DisconnectEvent{
		Client: c,
		Type:   DisconnectUser,
	})
}

func (c *Client) close(event *DisconnectEvent) error {
	c.closeMutex.Lock()
	defer c.closeMutex.Unlock()

	if c.connection == nil {
		return ErrState
	}
	c.end <- true
	c.connection.Close()
	c.connection = nil
	c.state = StateDisconnected
	c.users = nil
	c.channels = nil
	c.contextActions = nil
	c.self = nil
	c.audioEncoder = nil

	if listener := c.config.Listener; listener != nil {
		listener.OnDisconnect(event)
	}
	return nil
}

// Conn returns the underlying net.Conn to the server. Returns nil if the
// client is disconnected.
func (c *Client) Conn() net.Conn {
	if c.state == StateDisconnected {
		return nil
	}
	return c.connection
}

// State returns the current state of the client.
func (c *Client) State() State {
	return c.state
}

// Self returns a pointer to the User associated with the client. The function
// will return nil if the client has not yet been synced.
func (c *Client) Self() *User {
	return c.self
}

// Users returns a collection containing the users currently connected to the
// server.
func (c *Client) Users() Users {
	return c.users
}

// Channels returns a collection containing the server's channels.
func (c *Client) Channels() Channels {
	return c.channels
}

// ContextActions returns a collection containing the server's context actions.
func (c *Client) ContextActions() ContextActions {
	return c.contextActions
}

// SetVoiceTarget sets to whom transmitted audio will be sent. The VoiceTarget
// must have already been sent to the server for targeting to work correctly.
func (c *Client) SetVoiceTarget(target *VoiceTarget) {
	c.audioTarget = target
}

// Send will send a message to the server.
func (c *Client) Send(message Message) error {
	c.sendMutex.Lock()
	defer c.sendMutex.Unlock()

	if _, err := message.writeTo(c, c.connection); err != nil {
		return err
	}
	return nil
}
