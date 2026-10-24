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

// Output before the final write must stream except the held-back tail, which
// never leaks; the final write completes it and ends the scan.
func TestScanToSentinel_HoldsBackTail(t *testing.T) {
	cases := []struct {
		name        string
		writes      []string
		held, final string
	}{
		{"sentinel prefix", []string{"AB__EN", "D__\n"}, "AB", "AB"},
		{"split rune", []string{"m\xc2", "\xb2__END__\n"}, "m", "m²"},
		// Windows text-mode "\r\n" split across reads
		{"sentinel CR before LF", []string{"out__END__\r", "\n"}, "out", "out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{sentinel: "__END__"}
			pr, pw := io.Pipe()

			var mu sync.Mutex
			var got []string
			emit := func(data string, _ bool) { mu.Lock(); got = append(got, data); mu.Unlock() }
			joined := func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(got, "") }

			done := make(chan struct{})
			go func() {
				s.scanToSentinel(bufio.NewReader(pr), false, emit)
				close(done)
			}()

			last := len(tc.writes) - 1
			for _, w := range tc.writes[:last] {
				_, err := pw.Write([]byte(w))
				require.NoError(t, err)
			}
			require.Eventually(t, func() bool { return joined() == tc.held }, time.Second, 5*time.Millisecond)
			_, err := pw.Write([]byte(tc.writes[last]))
			require.NoError(t, err)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("scan did not return after sentinel")
			}
			require.Equal(t, tc.final, joined())
		})
	}
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
