// Command strategon-ca is an offline Ed25519 CA for Strategon mTLS bootstrap.
//
//	strategon-ca init --out ./ca/
//	strategon-ca sign --ca ./ca/ --cn m1 --out ./certs/m1/
//	strategon-ca sign --ca ./ca/ --cn control-plane --server --dns cp.internal --out ./certs/cp/
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/bullionbear/strategon/internal/ca"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "sign":
		runSign(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("out", "", "output directory for ca-cert.pem and ca-key.pem")
	_ = fs.Parse(args)
	bundle, err := ca.Init(*out)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("CA written:\n  %s\n  %s\n", bundle.CertPath, bundle.KeyPath)
}

func runSign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	caDir := fs.String("ca", "", "CA directory (ca-cert.pem, ca-key.pem)")
	out := fs.String("out", "", "output directory for cert.pem and key.pem")
	cn := fs.String("cn", "", "certificate CommonName (machine id for agents)")
	server := fs.Bool("server", false, "issue a server cert (ServerAuth); default is client (ClientAuth)")
	dns := fs.String("dns", "", "comma-separated DNS SANs (server certs; CN is also added as DNS SAN)")
	ip := fs.String("ip", "", "comma-separated IP SANs (server certs)")
	_ = fs.Parse(args)

	var ips []net.IP
	for _, s := range splitCSV(*ip) {
		parsed := net.ParseIP(s)
		if parsed == nil {
			fatal(fmt.Errorf("invalid --ip %q", s))
		}
		ips = append(ips, parsed)
	}

	bundle, err := ca.Sign(ca.SignOpts{
		CADir:    *caDir,
		OutDir:   *out,
		CN:       *cn,
		Server:   *server,
		DNSNames: splitCSV(*dns),
		IPs:      ips,
	})
	if err != nil {
		fatal(err)
	}
	kind := "client"
	if *server {
		kind = "server"
	}
	fmt.Printf("%s cert written (cn=%s):\n  %s\n  %s\n", kind, *cn, bundle.CertPath, bundle.KeyPath)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "strategon-ca: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintf(os.Stderr, `strategon-ca — offline Ed25519 CA for Strategon mTLS

Usage:
  strategon-ca init --out ./ca/
  strategon-ca sign --ca ./ca/ --cn m1 --out ./certs/m1/
  strategon-ca sign --ca ./ca/ --cn control-plane --server --dns cp.internal --ip 127.0.0.1 --out ./certs/cp/

Flags (sign):
  --ca       CA directory containing ca-cert.pem / ca-key.pem
  --cn       CommonName (agent machine id, or server name)
  --out      output directory for cert.pem / key.pem
  --server   issue ServerAuth cert (default: ClientAuth)
  --dns      comma-separated DNS SANs
  --ip       comma-separated IP SANs
`)
}
