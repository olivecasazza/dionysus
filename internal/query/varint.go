package query

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

// ─── varint helpers (Minecraft wire format) ────────────────────────────
//
// Minecraft uses LEB128 varints: 7 data bits per byte, high bit set
// means "more bytes follow". Integers are encoded little-endian-style
// (lowest 7 bits first). -1 is encoded as 0xFF 0xFF 0xFF 0xFF 0x0F.
//
// Reference: https://wiki.vg/Data_types#Varint_and_Varlong

// varint encodes an int32 as a Minecraft varint. Returns 1-5 bytes.
func varint(n int32) []byte {
	// Treat as uint32 so the sign bit becomes a high data bit rather
	// than an arithmetic shift. This is how Java (and the wiki)
	// define the encoding.
	u := uint32(n)
	var buf []byte
	for {
		// Take the lowest 7 bits.
		b := u & 0x7F
		u >>= 7
		if u != 0 {
			b |= 0x80 // continue bit
		}
		buf = append(buf, byte(b))
		if u == 0 {
			break
		}
	}
	return buf
}

// stringVarint encodes a string with a varint length prefix.
func stringVarint(s string) []byte {
	out := varint(int32(len(s)))
	out = append(out, []byte(s)...)
	return out
}

// ushort encodes a big-endian uint16 (Minecraft is BE for fixed-width).
func ushort(p uint16) []byte {
	return []byte{byte(p >> 8), byte(p & 0xFF)}
}

// packet wraps one or more payload chunks with a varint total length
// prefix. Callers concatenate payload pieces; packet computes the length.
func packet(parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	return append(varint(int32(len(body))), body...)
}

// readVarint reads a varint from r. Returns the value and the number of
// bytes consumed. Errors on overflow or short read.
func readVarint(r io.Reader) (int32, error) {
	var result uint32
	var shift uint
	for i := 0; i < 5; i++ {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		result |= uint32(b[0]&0x7F) << shift
		if b[0]&0x80 == 0 {
			return int32(result), nil
		}
		shift += 7
	}
	return 0, errors.New("varint too long")
}

// readResponse reads one framed SLP response: outer varint length, inner
// varint packet ID (must be 0), then varint-prefixed JSON string.
func readResponse(r io.Reader) (string, error) {
	// Outer packet length.
	if _, err := readVarint(r); err != nil {
		return "", err
	}
	// Packet ID (0 for Status Response).
	if _, err := readVarint(r); err != nil {
		return "", err
	}
	// JSON string length.
	strLen, err := readVarint(r)
	if err != nil {
		return "", err
	}
	if strLen < 0 {
		return "", errors.New("negative string length")
	}
	buf := make([]byte, strLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// parseSLPJSON extracts the player count and sample names from the SLP
// JSON response. Tolerates servers that omit sample or report null.
func parseSLPJSON(s string) (Result, error) {
	var resp slpResponse
	if err := json.NewDecoder(bytes.NewReader([]byte(s))).Decode(&resp); err != nil {
		return Result{}, err
	}
	out := Result{
		Online: resp.Players.Online,
		Max:    resp.Players.Max,
	}
	for _, p := range resp.Players.Sample {
		if p.Name != "" {
			out.Names = append(out.Names, p.Name)
		}
	}
	return out, nil
}

// binary.BigEndian is referenced for parity with the encoding used by
// ushort(); kept imported so future BE-fixed-width fields land cleanly.
var _ = binary.BigEndian
