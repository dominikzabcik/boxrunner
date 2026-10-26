package seen

import (
	"context"
	"fmt"
	"strings"

	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
)

// Table is the nftables table with the one rule that logs new connections. It
// is a table of its own, apart from the egress allowlists' table, so that
// every class is logged whether it has an allowlist or not.
const Table = "boxrunner_seen"

// Script is the nft script that makes the table what it should be. The chain
// hooks forward at priority 10, after the filter priority of 0 where
// iptables' rules and the egress table drop what a job may not reach, so only
// connections that were let through are logged. The rules cover the job
// network's bridge and every egress class's, and a limit keeps a scan from
// flooding the log.
func Script(group int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %[1]s\nadd chain inet %[1]s forward { type filter hook forward priority 10; policy accept; }\nflush chain inet %[1]s forward\n", Table)
	for _, bridge := range []string{layout.Bridge, firewall.BridgePrefix + "*"} {
		fmt.Fprintf(&b, "add rule inet %s forward iifname \"%s\" ct state new limit rate 100/second burst 200 packets log group %d snaplen 96\n", Table, bridge, group)
	}
	return b.String()
}

// Apply installs or replaces the rule in one transaction.
func Apply(ctx context.Context, run firewall.Run) error {
	if out, err := run(ctx, Script(layout.SeenNFLogGroup), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// Intact reports whether `nft list table` still shows the rules, as it does
// not after `nft flush ruleset` or a flush of the table.
func Intact(ctx context.Context, run firewall.Run) bool {
	out, err := run(ctx, "", "nft", "list", "table", "inet", Table)
	return err == nil && strings.Contains(out, "hook forward priority") &&
		strings.Contains(out, fmt.Sprintf("log group %d", layout.SeenNFLogGroup)) &&
		strings.Contains(out, `"`+layout.Bridge+`"`) && strings.Contains(out, `"`+firewall.BridgePrefix+`*"`)
}

// Remove deletes the table. A table that is not there counts as removed, and
// so does a box without nft.
func Remove(ctx context.Context, run firewall.Run) error {
	if _, err := run(ctx, "", "nft", "list", "table", "inet", Table); err != nil {
		return nil
	}
	if out, err := run(ctx, "", "nft", "delete", "table", "inet", Table); err != nil {
		return fmt.Errorf("nft delete table: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}
