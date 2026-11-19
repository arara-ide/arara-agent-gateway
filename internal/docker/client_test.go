package docker

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestDemuxSeparatesStreams(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "line one\n"))
	in.Write(frame(2, "warning\n"))
	in.Write(frame(1, "line two\n"))
	var out bytes.Buffer
	var errTail Tail
	if err := demux(&in, &out, &errTail); err != nil {
		t.Fatal(err)
	}
	if out.String() != "line one\nline two\n" || errTail.String() != "warning\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errTail.String())
	}
}

func TestTailKeepsOnlyTheEnd(t *testing.T) {
	var tail Tail
	tail.Write([]byte(strings.Repeat("a", tailLimit)))
	tail.Write([]byte("END"))
	s := tail.String()
	if len(s) != tailLimit || !strings.HasSuffix(s, "END") {
		t.Fatalf("len=%d", len(s))
	}
}
