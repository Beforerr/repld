package main

import (
	"bufio"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScanToSentinel_StreamsPartialLine(t *testing.T) {
	s := &Session{sentinel: "__END__"}
	pr, pw := io.Pipe()

	var mu sync.Mutex
	var got []string
	emit := func(data string, _ bool) { mu.Lock(); got = append(got, data); mu.Unlock() }
	joined := func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(got, "") }

	done := make(chan string, 1)
	go func() {
		tail, _ := s.scanToSentinel(bufio.NewReader(pr), false, emit)
		done <- tail
	}()

	_, err := pw.Write([]byte("DOT"))
	require.NoError(t, err)
	// Streams before any newline exists in the stream.
	require.Eventually(t, func() bool { return joined() == "DOT" }, time.Second, 5*time.Millisecond,
		"partial line should stream before the sentinel arrives")

	// Sentinel appended directly onto the same partial line must terminate the
	// scan and never leak into user output.
	_, err = pw.Write([]byte("__END__\n"))
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan did not return after sentinel")
	}
	require.Equal(t, "DOT", joined(), "sentinel must be stripped, not emitted")
}

// partial sentinel never reaches users, while everything before it streams
func TestScanToSentinel_HoldsBackSentinelPrefix(t *testing.T) {
	s := &Session{sentinel: "__END__"}
	pr, pw := io.Pipe()

	var mu sync.Mutex
	var got []string
	emit := func(data string, _ bool) { mu.Lock(); got = append(got, data); mu.Unlock() }
	joined := func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(got, "") }

	done := make(chan string, 1)
	go func() {
		tail, _ := s.scanToSentinel(bufio.NewReader(pr), false, emit)
		done <- tail
	}()

	_, err := pw.Write([]byte("AB__EN"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return joined() == "AB" }, time.Second, 5*time.Millisecond)
	// Give a beat to ensure the held-back prefix is not flushed late.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, "AB", joined(), "sentinel prefix must not leak")

	_, err = pw.Write([]byte("D__\n")) // completes the sentinel
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan did not return after sentinel")
	}
	require.Equal(t, "AB", joined())
}

func TestScanToSentinel_LinesAndTail(t *testing.T) {
	s := &Session{sentinel: "__END__"}
	r := bufio.NewReader(strings.NewReader("alpha\nbeta\n__END__\n"))

	var got []string
	emit := func(data string, _ bool) { got = append(got, data) }
	tail, err := s.scanToSentinel(r, false, emit)
	require.NoError(t, err)
	require.Equal(t, "alpha\nbeta\n", strings.Join(got, ""))
	require.Equal(t, "alpha\nbeta\n", tail)
}

func TestScanToSentinel_EOFFlushesPartial(t *testing.T) {
	s := &Session{sentinel: "__END__"}
	r := bufio.NewReader(strings.NewReader("partial-no-newline"))

	var got []string
	emit := func(data string, _ bool) { got = append(got, data) }
	tail, err := s.scanToSentinel(r, false, emit)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, "partial-no-newline", strings.Join(got, ""))
	require.Equal(t, "partial-no-newline", tail)
}
