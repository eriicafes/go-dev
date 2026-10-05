package vite

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type socket struct {
	connection net.Conn
	reader     *bufio.Reader
	writeMu    sync.Mutex
}

func dial(ctx context.Context, endpoint *url.URL) (*socket, error) {
	dialer := &net.Dialer{}
	var connection net.Conn
	var err error
	if endpoint.Scheme == "wss" {
		connection, err = tls.DialWithDialer(dialer, "tcp", endpoint.Host, &tls.Config{ServerName: endpoint.Hostname()})
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", endpoint.Host)
	}
	if err != nil {
		return nil, err
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = connection.Close()
		return nil, err
	}
	request := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: endpoint.EscapedPath(), RawQuery: endpoint.RawQuery},
		Host:   endpoint.Host,
		Header: http.Header{
			"Connection":             {"Upgrade"},
			"Upgrade":                {"websocket"},
			"Sec-Websocket-Version":  {"13"},
			"Sec-Websocket-Key":      {base64.StdEncoding.EncodeToString(keyBytes)},
			"Sec-Websocket-Protocol": {"vite-hmr"},
		},
	}
	if err := request.Write(connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
		_ = response.Body.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("unexpected WebSocket response %s", response.Status)
	}
	return &socket{connection: connection, reader: reader}, nil
}

func (socket *socket) readText() ([]byte, error) {
	for {
		first, err := socket.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		second, err := socket.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		opcode := first & 0x0f
		masked := second&0x80 != 0
		length, err := socket.frameLength(second & 0x7f)
		if err != nil {
			return nil, err
		}
		if length > 16<<20 {
			return nil, errors.New("Vite HMR frame exceeds 16 MiB")
		}
		mask := make([]byte, 4)
		if masked {
			if _, err := io.ReadFull(socket.reader, mask); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(socket.reader, payload); err != nil {
			return nil, err
		}
		if masked {
			for index := range payload {
				payload[index] ^= mask[index%4]
			}
		}
		switch opcode {
		case 0x1:
			return payload, nil
		case 0x8:
			return nil, io.EOF
		case 0x9:
			if err := socket.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
		case 0xA:
			continue
		default:
			return nil, fmt.Errorf("unsupported Vite HMR WebSocket opcode %d", opcode)
		}
	}
}

func (socket *socket) frameLength(lengthByte byte) (int, error) {
	switch lengthByte {
	case 126:
		var length uint16
		if err := binaryRead(socket.reader, &length); err != nil {
			return 0, err
		}
		return int(length), nil
	case 127:
		var length uint64
		if err := binaryRead(socket.reader, &length); err != nil {
			return 0, err
		}
		if length > 16<<20 {
			return 0, errors.New("Vite HMR frame exceeds 16 MiB")
		}
		return int(length), nil
	default:
		return int(lengthByte), nil
	}
}

func (socket *socket) writeFrame(opcode byte, payload []byte) error {
	socket.writeMu.Lock()
	defer socket.writeMu.Unlock()
	if len(payload) > 125 {
		return errors.New("control frame payload exceeds 125 bytes")
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	frame = append(frame, mask...)
	for index, value := range payload {
		frame = append(frame, value^mask[index%4])
	}
	_, err := socket.connection.Write(frame)
	return err
}

func (socket *socket) close() { _ = socket.connection.Close() }

func binaryRead(reader io.Reader, value any) error {
	var bytes [8]byte
	size := 2
	if _, ok := value.(*uint64); ok {
		size = 8
	}
	if _, err := io.ReadFull(reader, bytes[:size]); err != nil {
		return err
	}
	switch value := value.(type) {
	case *uint16:
		*value = uint16(bytes[0])<<8 | uint16(bytes[1])
	case *uint64:
		for _, b := range bytes[:8] {
			*value = *value<<8 | uint64(b)
		}
	}
	return nil
}
