package query

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	gamesv1alpha1 "github.com/olivecasazza/game-operator/api/v1alpha1"
)

// Minecraft speaks the modern (1.7+) Server List Ping protocol over TCP.
// Reference: https://wiki.vg/Server_List_Ping
type Minecraft struct {
	addr    string
	timeout time.Duration
}

// NewMinecraft constructs a Minecraft SLP client with the default timeout.
func NewMinecraft(spec gamesv1alpha1.QuerySpec) *Minecraft {
	return &Minecraft{addr: addrFromSpec(spec), timeout: defaultTimeout}
}

// Query performs the SLP handshake → request → response sequence and
// parses the JSON players block into Result.
func (m *Minecraft) Query(ctx context.Context) (Result, error) {
	dialer := net.Dialer{Timeout: m.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return Result{}, fmt.Errorf("mc dial %s: %w", m.addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(m.timeout))

	host, port := splitHostPort(m.addr)

	// Handshake packet: varint packet ID 0, varint protocol -1 (ping),
	// varint+host string, ushort port, varint next state 1 (status).
	handshake := packet(
		varint(0),          // packet ID
		varint(-1),         // protocol version (-1 = ping)
		stringVarint(host), // server host (for virtual hosting)
		ushort(port),       // server port
		varint(1),          // next state = Status
	)
	if _, err := conn.Write(handshake); err != nil {
		return Result{}, fmt.Errorf("mc handshake write: %w", err)
	}

	// Status request packet: varint packet ID 0, no payload.
	request := packet(varint(0))
	if _, err := conn.Write(request); err != nil {
		return Result{}, fmt.Errorf("mc request write: %w", err)
	}

	// Response: framed by a varint length prefix. Inner: varint packet ID
	// (0) + varint-prefixed JSON string.
	jsonStr, err := readResponse(conn)
	if err != nil {
		return Result{}, fmt.Errorf("mc response read: %w", err)
	}

	res, err := parseSLPJSON(jsonStr)
	if err != nil {
		return Result{}, fmt.Errorf("mc response parse: %w", err)
	}
	return res, nil
}

// slpResponse is the subset of the SLP JSON we care about.
type slpResponse struct {
	Players slpPlayers `json:"players"`
}

type slpPlayers struct {
	Max    int32          `json:"max"`
	Online int32          `json:"online"`
	Sample []slpPlayerRef `json:"sample"`
}

type slpPlayerRef struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// splitHostPort wraps net.SplitHostPort returning the port as uint16
// (the on-wire ushort type). Errors are unreachable here because
// addrFromSpec always produces host:port, but we panic to surface any
// caller bug loudly rather than silently mangling the request.
func splitHostPort(addr string) (string, uint16) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		panic(fmt.Sprintf("query.splitHostPort: bad addr %q: %v", addr, err))
	}
	var port16 uint16
	port32, err := net.LookupPort("tcp", portStr)
	if err != nil {
		panic(fmt.Sprintf("query.splitHostPort: bad port %q: %v", portStr, err))
	}
	port16 = uint16(port32)
	return host, port16
}

// ushort is a guard so encoding/binary stays imported — currently unused
// (we encode the port manually via byte math in ushort()) but binary
// remains in place for future Minecraft packet extensions.
var _ = binary.BigEndian

// errShortRead is returned when the connection returned EOF mid-packet.
var errShortRead = errors.New("short read")

// io.EOF alias for clarity at call sites.
var _ = io.EOF
