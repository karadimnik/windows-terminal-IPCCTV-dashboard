package relay

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// rtspDescribe sends an anonymous RTSP DESCRIBE and returns the status line.
func rtspDescribe(t *testing.T, port int, path string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "DESCRIBE rtsp://127.0.0.1:%d/%s RTSP/1.0\r\nCSeq: 1\r\nAccept: application/sdp\r\n\r\n", port, path)
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}
