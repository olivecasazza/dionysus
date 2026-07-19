package query

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// A2S speaks the Valve A2S_PLAYER + A2S_INFO protocols over UDP.
// Reference: https://developer.valvesoftware.com/wiki/Server_queries
type A2S struct {
	addr    string
	timeout time.Duration
}

// NewA2S constructs an A2S client. Uses the default 5s timeout; override
// the struct field directly in tests.
func NewA2S(spec gamesv1alpha1.QuerySpec) *A2S {
	return &A2S{addr: addrFromSpec(spec), timeout: defaultTimeout}
}

// Magic prefixes for the A2S protocol.
var (
	// a2sHeader is the 4-byte -1 header that prefixes every simple query
	// response. Replies with a different header are multi-packet (we don't
	// handle those — game servers small enough for single-packet replies).
	a2sHeader = []byte{0xFF, 0xFF, 0xFF, 0xFF}

	// a2sPlayer is the A2S_PLAYER opcode.
	a2sPlayer byte = 0x55

	// a2sPlayerResponse is the A2S_PLAYER reply opcode.
	a2sPlayerResponse byte = 0x44

	// a2sChallenge is the S2C_CHALLENGE opcode the server sends when it
	// wants us to re-send the request with its challenge token.
	a2sChallenge byte = 0x41

	// a2sInfo is the A2S_INFO opcode (Source Engine Query payload).
	a2sInfo = append([]byte{0x54}, []byte("Source Engine Query\x00")...)

	// a2sInfoResponse is the A2S_INFO reply opcode (Source-style; GoldSrc
	// uses 0x6D — not handled because every supported target is Source).
	a2sInfoResponse byte = 0x49
)

// Query issues A2S_INFO (to get max players) then A2S_PLAYER (for the
// current count and names). Both share a single UDP connection; the
// server's challenge handshake is handled transparently.
func (a *A2S) Query(ctx context.Context) (Result, error) {
	dialer := net.Dialer{Timeout: a.timeout}
	conn, err := dialer.DialContext(ctx, "udp", a.addr)
	if err != nil {
		return Result{}, fmt.Errorf("a2s dial %s: %w", a.addr, err)
	}
	defer conn.Close()

	// Bound each read by the per-call timeout. The parent ctx also covers
	// cancellation, but UDP reads don't honor ctx on all platforms.
	_ = conn.SetDeadline(time.Now().Add(a.timeout))

	info, err := a.exchangeInfo(conn)
	if err != nil {
		return Result{}, fmt.Errorf("a2s info: %w", err)
	}

	players, err := a.exchangePlayers(conn)
	if err != nil {
		// Some servers refuse A2S_PLAYER even after a successful INFO.
		// Degrade gracefully: report the INFO max + online=0 rather than
		// failing the whole reconcile.
		return Result{Online: 0, Max: info.max}, nil
	}

	return Result{
		Online: players.count,
		Max:    info.max,
		Names:  players.names,
	}, nil
}

// infoResult is the parsed subset of an A2S_INFO reply we care about.
type infoResult struct {
	max int32
}

// exchangeInfo sends A2S_INFO and parses the response. The server may
// reply with a challenge instead of the info packet; if so, re-send the
// request with the challenge token appended (some servers require this).
func (a *A2S) exchangeInfo(conn net.Conn) (infoResult, error) {
	if _, err := conn.Write(appendHeader(a2sInfo)); err != nil {
		return infoResult{}, err
	}
	resp, err := readPacket(conn)
	if err != nil {
		return infoResult{}, err
	}

	// If the server returned a challenge, re-send INFO with the challenge.
	if len(resp) >= 5 && resp[4] == a2sChallenge {
		challenge := resp[5:9]
		payload := append(appendHeader(a2sInfo), challenge...)
		if _, err := conn.Write(payload); err != nil {
			return infoResult{}, err
		}
		resp, err = readPacket(conn)
		if err != nil {
			return infoResult{}, err
		}
	}

	return parseInfo(resp)
}

// parseInfo extracts the max-players field from an A2S_INFO response.
// Layout (Source): header(4) + 0x49 + Protocol(1) + Name(cstr) +
// Folder(cstr) + Game(cstr) + AppID(2) + Players(1) + Max(1) + ...
func parseInfo(resp []byte) (infoResult, error) {
	if len(resp) < 5 || resp[4] != a2sInfoResponse {
		return infoResult{}, fmt.Errorf("unexpected a2s_info response header: %x", resp[4:])
	}
	// Skip header(4) + type(1), then walk three C-strings.
	body := resp[5:]
	body, err := skipCString(body) // server name
	if err != nil {
		return infoResult{}, err
	}
	body, err = skipCString(body) // folder
	if err != nil {
		return infoResult{}, err
	}
	body, err = skipCString(body) // game
	if err != nil {
		return infoResult{}, err
	}
	// body is now: AppID(2 le) + Players(1) + Max(1) + ...
	if len(body) < 4 {
		return infoResult{}, errors.New("a2s_info response truncated")
	}
	max := int32(body[3])
	return infoResult{max: max}, nil
}

// playersResult is the parsed subset of an A2S_PLAYER reply.
type playersResult struct {
	count int32
	names []string
}

// exchangePlayers sends A2S_PLAYER (with challenge retry), parses the
// per-player records into count + names.
func (a *A2S) exchangePlayers(conn net.Conn) (playersResult, error) {
	// Initial request with a sentinel challenge of -1. The server almost
	// always responds with a real challenge; we then resend with it.
	req := appendHeader([]byte{a2sPlayer, 0xFF, 0xFF, 0xFF, 0xFF})
	if _, err := conn.Write(req); err != nil {
		return playersResult{}, err
	}
	resp, err := readPacket(conn)
	if err != nil {
		return playersResult{}, err
	}

	// Resolve the challenge and re-send.
	if len(resp) >= 5 && resp[4] == a2sChallenge {
		challenge := resp[5:9]
		req = append(appendHeader([]byte{a2sPlayer}), challenge...)
		if _, err := conn.Write(req); err != nil {
			return playersResult{}, err
		}
		resp, err = readPacket(conn)
		if err != nil {
			return playersResult{}, err
		}
	}

	return parsePlayers(resp)
}

// parsePlayers extracts count + per-player names from A2S_PLAYER reply.
// Layout: header(4) + 0x44 + Count(1) + per-player[ Index(1),
// Name(cstr), Score(4 le), Duration(4 float) ].
func parsePlayers(resp []byte) (playersResult, error) {
	if len(resp) < 5 || resp[4] != a2sPlayerResponse {
		return playersResult{}, fmt.Errorf("unexpected a2s_player response header: %x", resp[4:])
	}
	count := int32(resp[5])
	body := resp[6:]
	out := playersResult{count: count, names: make([]string, 0, count)}

	for i := int32(0); i < count; i++ {
		if len(body) < 1 {
			break
		}
		body = body[1:] // index byte (unused)
		name, rest, err := readCString(body)
		if err != nil {
			break
		}
		body = rest
		if len(body) < 8 {
			break
		}
		// Skip score(4) + duration(4).
		body = body[8:]
		out.names = append(out.names, name)
	}
	return out, nil
}

// appendHeader prepends the -1 single-packet header to the payload.
func appendHeader(payload []byte) []byte {
	return append(append([]byte{}, a2sHeader...), payload...)
}

// readPacket reads one UDP packet (capped at 1400 bytes, the standard A2S
// MTU). Single-packet replies fit in one read; multi-packet replies
// would require reassembly that this implementation deliberately omits
// (game servers small enough to fit).
func readPacket(conn net.Conn) ([]byte, error) {
	buf := make([]byte, 1400)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// readCString reads a NUL-terminated string from the front of buf.
// Returns the string and the remaining slice.
func readCString(buf []byte) (string, []byte, error) {
	idx := bytes.IndexByte(buf, 0)
	if idx < 0 {
		return "", nil, errors.New("missing string terminator")
	}
	return string(buf[:idx]), buf[idx+1:], nil
}

// skipCString advances past one C-string in buf.
func skipCString(buf []byte) ([]byte, error) {
	idx := bytes.IndexByte(buf, 0)
	if idx < 0 {
		return nil, errors.New("missing string terminator")
	}
	return buf[idx+1:], nil
}

// Unused import guard: binary is imported for future protocol extensions
// (e.g. multi-packet reassembly uses binary.LittleEndian for packet
// numbers). Keeps go vet quiet when those land.
var _ = binary.LittleEndian
