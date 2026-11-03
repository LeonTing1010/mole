package cmd

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// `mole server link`: the one link worth handing to another client.
//
// What Server.URI() prints is not a share link, and cannot be made into one. It
// is mole's own interchange format because it has to carry things a share link
// has no room for: the Brutal ceiling ladder, the peak window, the port-hop
// range, and the pinned certificate. Every one of them rides in a query
// parameter only mole's parser reads (config/parser.go), so a third-party
// importer drops all of them silently — and the pinned node is the case that
// actually breaks rather than merely degrades. mole omits `insecure=1`
// precisely because it pins, so the importer is left verifying a self-signed
// certificate against the public roots: the handshake fails, and nothing in the
// error mentions the parameter it never saw. Hiddify's ray2sing reads exactly
// five hysteria2 parameters — obfs, obfs-password, sni, insecure, ech — and
// `pinnedcert` is not among them.
//
// The REALITY leg has none of that problem, which is why this command prints
// only it. Its link is the standard vless:// shape every client already speaks,
// and REALITY authenticates with the X25519 key in ?pbk= instead of a
// certificate chain — so there is no pin to carry and nothing is downgraded by
// the link leaving mole. This is what to give Hiddify, v2rayN or NekoBox.
//
// What it does not carry is the ceiling. A client on this link negotiates its
// own congestion control and never sees `mole ceiling`, so it will not track the
// ladder or the time-of-day schedule. That is the price of portability, and the
// reason the hy2 leg stays mole's own.
var serverLinkCmd = &cobra.Command{
	Use:   "link [name]",
	Short: "Print the vless:// REALITY link another client can import",
	Long: `Print a link a client other than mole can use.

By default this is the VLESS+REALITY leg. The hysteria2 link mole runs on
carries the pinned certificate, the Brutal ladder and the peak window in
parameters no other client reads, so importing it elsewhere yields a node that
fails its TLS verification and a bandwidth ceiling nobody honoured.

REALITY needs no certificate: it authenticates with the public key already in
the link, so nothing is lost by handing it out. What is lost is the ceiling —
another client negotiates its own congestion control and does not follow
"mole ceiling".

The uuid in the link is the credential. Anyone holding it can use the node.

--hy2 prints the hysteria2 leg instead, in the plain dialect every client
speaks. That is only possible with verification off, so the flag says so and
the output repeats it; see portableHy2Link for what it costs.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLink,
}

var linkHy2 bool

func init() {
	serverCmd.AddCommand(serverLinkCmd)
	serverLinkCmd.Flags().BoolVar(&linkHy2, "hy2", false,
		"print the hysteria2 leg as a standard link — certificate verification off, because a pin has no standard spelling")
}

func runLink(_ *cobra.Command, args []string) error {
	srv, err := linkTarget(args)
	if err != nil {
		return err
	}
	if linkHy2 {
		return printPortableHy2(srv)
	}
	link, leg, err := portableLink(srv)
	if err != nil {
		return err
	}

	fmt.Printf("🎭 %s — VLESS+REALITY, %s\n\n", srv.Name, leg)
	fmt.Println(link)
	fmt.Println("\n   Any client that speaks vless imports this as-is (Hiddify, v2rayN, NekoBox).")
	fmt.Println("   The uuid in it is the credential; the Brutal ceiling is not in it, so that")
	fmt.Println("   client negotiates its own congestion control and ignores `mole ceiling`.")
	return nil
}

// portableLink is the whole decision this command makes: which of a node's legs
// can be handed to a client that is not mole, and why. Pure so the three node
// shapes can be tested without a servers.json to read.
func portableLink(srv *Server) (link, leg string, err error) {
	switch {
	case srv.hasStandbyLeg():
		return srv.StandbyURI(), "the TCP leg beside hysteria2", nil
	case srv.isReality():
		return srv.URI(), "this node's only leg", nil
	default:
		return "", "", fmt.Errorf("server %q has no REALITY leg, and its hysteria2 link is mole's own format — the pinned certificate rides in a parameter no other client reads, so there is nothing portable to hand out. `mole server repair %s` installs a REALITY leg", srv.Name, srv.Name)
	}
}

func printPortableHy2(srv *Server) error {
	if srv.isReality() {
		return fmt.Errorf("server %q is a VLESS+REALITY node and has no hysteria2 leg", srv.Name)
	}
	if srv.Password == "" {
		return fmt.Errorf("server %q has no tunnel password on file — it was adopted without one", srv.Name)
	}
	fmt.Printf("⚠️  %s — hysteria2 on UDP/%d, certificate verification OFF\n\n", srv.Name, srv.Port)
	fmt.Println(portableHy2Link(srv))
	fmt.Println("\n   A standard link: Hiddify, v2rayN and NekoBox import it as-is.")
	fmt.Println("   It has to say insecure=1 — a pin has no standard spelling, and without the")
	fmt.Println("   flag the importer verifies a self-signed certificate against the public")
	fmt.Println("   roots and fails. Hysteria2 sends its password inside that session, so the")
	fmt.Println("   salamander key in this line is the only thing between an on-path peer and")
	fmt.Println("   the tunnel password. Treat it as a secret.")
	fmt.Println("   The Brutal ceiling does not travel either: this client negotiates its own.")
	fmt.Println("   `mole server link` without --hy2 gives the verified REALITY leg instead.")
	return nil
}

// portableHy2Link renders the hysteria2 leg in the plain share-link dialect
// every other client speaks, which means trading the pin for insecure=1.
//
// Server.URI() cannot be reduced to this without losing something real. It
// carries the certificate in ?pinnedcert= and, because it pins, deliberately
// omits insecure=1; an importer reads neither — Hiddify's ray2sing reads exactly
// obfs, obfs-password, sni, insecure and ech — so it keeps a pin it cannot see
// and drops the flag that would have let it connect. What arrives is a link that
// imports cleanly and then fails its handshake against a self-signed
// certificate, with nothing in the error naming the parameter it never saw.
//
// Verification off is not free, which is why this is behind a flag rather than
// the default: the salamander key travels in the same line, and with nothing
// verifying the far end, whoever holds that key can also hold the handshake and
// take the password hysteria2 sends inside it. Pinning is what makes a leaked
// obfs key insufficient.
//
// Only the parameters a client can act on are emitted. The ceiling, the peak
// window and the hop range are left out rather than included as decoration —
// importers drop unknown parameters silently, and a number that looks like it
// travelled is worse than one that is visibly absent.
func portableHy2Link(srv *Server) string {
	q := url.Values{}
	q.Set("insecure", "1")
	// Only when known. A node with no SNI on file predates pinning and its
	// certificate says bing.com; Server.URI() falls back to that name, but
	// handing it to a third-party client would re-publish the impersonation
	// pinning retired, and with verification off the client does not need an SNI
	// to connect.
	if srv.SNI != "" {
		q.Set("sni", srv.SNI)
	}
	if srv.Obfs != "" {
		q.Set("obfs", srv.Obfs)
		if srv.ObfsPassword != "" {
			q.Set("obfs-password", srv.ObfsPassword)
		}
	}
	return fmt.Sprintf("hysteria2://%s@%s:%d?%s#%s", srv.Password, srv.linkHost(), srv.Port, q.Encode(), srv.Name)
}

// linkTarget resolves the named server, or the active one. It deliberately does
// not go through ActiveServer(): that refuses a halted node, and printing a link
// is not a connection attempt — the operator may well want the link for a node
// they are about to start. It does refuse a node with no address, because the
// link would have an empty host in it.
func linkTarget(args []string) (*Server, error) {
	servers, err := loadServers()
	if err != nil {
		return nil, fmt.Errorf("read local servers: %w", err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers deployed — run `mole server deploy`")
	}
	if len(args) == 1 {
		idx := indexByName(servers, args[0])
		if idx < 0 {
			return nil, fmt.Errorf("server %q not found", args[0])
		}
		if !servers[idx].HasAddress() {
			return nil, fmt.Errorf("server %q has no address yet — `mole server reconcile` fills it in", servers[idx].Name)
		}
		return &servers[idx], nil
	}
	for i := range servers {
		if servers[i].Active {
			if !servers[i].HasAddress() {
				return nil, fmt.Errorf("server %q has no address yet — `mole server reconcile` fills it in", servers[i].Name)
			}
			return &servers[i], nil
		}
	}
	return nil, fmt.Errorf("no active server — pick one with `mole server use <name>`")
}
