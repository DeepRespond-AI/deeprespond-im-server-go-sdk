package main

// 桥接协议边界：拒绝跨来源、非法长度和掩码，控制帧与业务帧共用写锁。

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type memoryConn struct{}

func (memoryConn) Close() error                     { return nil }
func (memoryConn) SetWriteDeadline(time.Time) error { return nil }

func maskedFrame(op byte, payload []byte) []byte {
	out := []byte{op, 0x80 | byte(len(payload)), 1, 2, 3, 4}
	for i, b := range payload {
		out = append(out, b^byte(i%4+1))
	}
	return out
}

func memoryBridge(input []byte, output *bytes.Buffer) *bridgeConn {
	return &bridgeConn{rwc: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(input)), bufio.NewWriter(output)), nc: memoryConn{}}
}

func TestBridgeRejectsMalformedFrames(t *testing.T) {
	negative := []byte{0x81, 0xff}
	negative = binary.BigEndian.AppendUint64(negative, 1<<63)
	oversized := []byte{0x81, 0xff}
	oversized = binary.BigEndian.AppendUint64(oversized, (8<<20)+1)
	for name, frame := range map[string][]byte{
		"无掩码": {0x81, 0}, "负长度": negative, "帧过大": oversized, "无起始分片": maskedFrame(0x80, []byte("x")), "控制帧分片": maskedFrame(0x09, nil),
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if _, err := memoryBridge(frame, &output).ReadMessage(); err == nil {
				t.Fatal("非法帧未被拒绝")
			}
		})
	}
}

func TestBridgeFragmentationAndConcurrentControlWrites(t *testing.T) {
	input := append(maskedFrame(0x01, []byte("he")), maskedFrame(0x89, []byte("ping"))...)
	input = append(input, maskedFrame(0x80, []byte("llo"))...)
	var output bytes.Buffer
	b := memoryBridge(input, &output)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if err := b.WriteMessage([]byte("reply")); err != nil {
				t.Error(err)
			}
		}
	}()
	message, err := b.ReadMessage()
	wg.Wait()
	if err != nil || string(message) != "hello" {
		t.Fatal(string(message), err)
	}
	// 无掩码服务端帧必须保持完整，ping 与业务回复不能互相穿插。
	raw := output.Bytes()
	frames := 0
	for len(raw) > 0 {
		if len(raw) < 2 {
			t.Fatal("响应帧头被破坏")
		}
		size := int(raw[1])
		if size > 125 || len(raw) < size+2 {
			t.Fatal("响应帧被并发写坏")
		}
		raw = raw[size+2:]
		frames++
	}
	if frames != 101 {
		t.Fatal("控制帧或回复丢失", frames)
	}
}

func TestBridgeRejectsForeignOrigin(t *testing.T) {
	req := httptest.NewRequest("GET", "http://localhost:8092/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "https://other.example")
	w := httptest.NewRecorder()
	if _, err := acceptBridge(w, req); err == nil || w.Code != 403 {
		t.Fatal(w.Code, err)
	}
}
