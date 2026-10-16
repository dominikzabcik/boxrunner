package nflog_test

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/nflog"
)

// TestReaderReceivesWhatNftablesLogs makes nftables log the SYN of a refused
// connection to a group and reads it back. It needs root and nft, so it runs
// only with BOXRUNNER_NFLOG_TEST=1.
func TestReaderReceivesWhatNftablesLogs(t *testing.T) {
	if os.Getenv("BOXRUNNER_NFLOG_TEST") != "1" {
		t.Skip("set BOXRUNNER_NFLOG_TEST=1 as root on a Linux box with nft")
	}
	const table = "boxrunner_nflog_test"
	script := `add table inet ` + table + `
add chain inet ` + table + ` out { type filter hook output priority 0; policy accept; }
add rule inet ` + table + ` out tcp dport 9 log group 1999 prefix "probe"
`
	nft := exec.Command("nft", "-f", "-")
	nft.Stdin = strings.NewReader(script)
	if out, err := nft.CombinedOutput(); err != nil {
		t.Fatalf("nft: %v: %s", err, out)
	}
	defer exec.Command("nft", "delete", "table", "inet", table).Run()

	r, err := nflog.Open(1999)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := make(chan nflog.Packet, 1)
	go func() {
		packets, err := r.Read()
		if err == nil && len(packets) > 0 {
			got <- packets[0]
		}
	}()
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:9", time.Second); err == nil {
		conn.Close()
	}
	select {
	case p := <-got:
		if p.Prefix != "probe" || p.Dst.String() != "127.0.0.1" || p.DstPort != 9 || p.Proto != 6 {
			t.Errorf("logged %+v, want the SYN to 127.0.0.1:9 with prefix probe", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no packet arrived within five seconds")
	}
}
