// Command burrow is the tunnel agent. It runs next to a local service and keeps
// one multiplexed connection open to a burrowd server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/suro4ek/burrow/internal/client"
	"github.com/suro4ek/burrow/internal/proto"
)

// version is overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "burrow: "+err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		usage()
		return nil
	}

	cmd, args := argv[0], argv[1:]
	switch cmd {
	case "http":
		return cmdHTTP(args)
	case "tcp":
		return cmdTCP(args)
	case "ssh":
		return cmdSSH(args)
	case "start":
		return cmdStart(args)
	case "login":
		return cmdLogin(args)
	case "logout":
		return cmdLogout(args)
	case "config":
		return cmdConfig(args)
	case "version", "--version", "-version":
		fmt.Println("burrow", version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// connFlags are the flags every tunnelling command shares.
type connFlags struct {
	server   *string
	token    *string
	noTLS    *bool
	insecure *bool
	tlsName  *string
	logLevel *string
}

// registerConn adds the shared connection flags to a flag set.
func registerConn(fs *flag.FlagSet) *connFlags {
	return &connFlags{
		server:   fs.String("server", "", "tunnel server control endpoint, host:port (default: saved login, then BURROW_SERVER)"),
		token:    fs.String("token", "", "agent token (default: saved login, then BURROW_TOKEN)"),
		noTLS:    fs.Bool("no-tls", false, "connect without TLS (only on a trusted network)"),
		insecure: fs.Bool("insecure", false, "skip server certificate verification"),
		tlsName:  fs.String("tls-name", "", "server name to verify in the certificate"),
		logLevel: fs.String("log-level", "info", "debug, info, warn or error"),
	}
}

// resolve merges flags, the saved login and the environment, in that order of
// precedence, and returns a ready client config.
func (c *connFlags) resolve(tunnels []client.TunnelSpec) (client.Config, error) {
	saved, err := client.LoadFileConfig()
	if err != nil {
		return client.Config{}, err
	}

	server := firstNonEmpty(*c.server, saved.Server, os.Getenv("BURROW_SERVER"))
	token := firstNonEmpty(*c.token, saved.Token, os.Getenv("BURROW_TOKEN"))
	if server == "" {
		return client.Config{}, fmt.Errorf("no server configured: run `burrow login -server host:port -token ...`")
	}
	if token == "" {
		return client.Config{}, fmt.Errorf("no token configured: run `burrow login -server host:port -token ...`")
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*c.logLevel)); err != nil {
		return client.Config{}, fmt.Errorf("invalid log level %q", *c.logLevel)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	useTLS := !(*c.noTLS || saved.NoTLS)
	insecure := *c.insecure || saved.Insecure
	if insecure {
		log.Warn("certificate verification disabled: the connection is encrypted but not authenticated")
	}

	return client.Config{
		ServerAddr:    server,
		Token:         token,
		TLS:           useTLS,
		TLSServerName: firstNonEmpty(*c.tlsName, saved.TLSName),
		Insecure:      insecure,
		Tunnels:       tunnels,
		Version:       version,
		Log:           log,
		OnReady:       func(g []proto.TunnelResp) { printGranted(server, g) },
	}, nil
}

// serve runs the agent until interrupted.
func serve(cfg client.Config) error {
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return c.Run(ctx)
}

func cmdHTTP(args []string) error {
	fs := flag.NewFlagSet("burrow http", flag.ExitOnError)
	conn := registerConn(fs)
	subdomain := fs.String("subdomain", "", "requested subdomain (default: a random one)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: burrow http <port|host:port> [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one port or host:port, got %d arguments", len(rest))
	}
	local, err := client.ParseLocalAddr(rest[0])
	if err != nil {
		return err
	}

	cfg, err := conn.resolve([]client.TunnelSpec{{
		Proto:     proto.ProtoHTTP,
		LocalAddr: local,
		Subdomain: strings.ToLower(*subdomain),
	}})
	if err != nil {
		return err
	}
	return serve(cfg)
}

func cmdTCP(args []string) error {
	fs := flag.NewFlagSet("burrow tcp", flag.ExitOnError)
	conn := registerConn(fs)
	port := fs.Int("port", 0, "requested public port (default: reserved port for your token, else a free one)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: burrow tcp <port|host:port> [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one port or host:port, got %d arguments", len(rest))
	}
	local, err := client.ParseLocalAddr(rest[0])
	if err != nil {
		return err
	}

	cfg, err := conn.resolve([]client.TunnelSpec{{
		Proto:      proto.ProtoTCP,
		LocalAddr:  local,
		RemotePort: *port,
	}})
	if err != nil {
		return err
	}
	return serve(cfg)
}

// cmdSSH is cmdTCP pointed at the local SSH daemon, which is the case people
// actually reach for.
func cmdSSH(args []string) error {
	fs := flag.NewFlagSet("burrow ssh", flag.ExitOnError)
	conn := registerConn(fs)
	local := fs.String("local", "22", "local SSH port or host:port")
	port := fs.Int("port", 0, "requested public port (default: reserved port for your token, else a free one)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: burrow ssh [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		fs.Usage()
		return fmt.Errorf("burrow ssh takes no positional arguments; use -local to change the local port")
	}
	addr, err := client.ParseLocalAddr(*local)
	if err != nil {
		return err
	}
	warnIfNoSSHD(addr)

	cfg, err := conn.resolve([]client.TunnelSpec{{
		Proto:      proto.ProtoTCP,
		LocalAddr:  addr,
		RemotePort: *port,
	}})
	if err != nil {
		return err
	}
	return serve(cfg)
}

// cmdStart publishes several tunnels over one connection.
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("burrow start", flag.ExitOnError)
	conn := registerConn(fs)
	var specs specList
	fs.Var(&specs, "tunnel", "tunnel in compact form, repeatable (e.g. http:3000:myapp, tcp:22:25343)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: burrow start -tunnel http:3000:myapp -tunnel tcp:22 [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q; every tunnel goes in a -tunnel flag", rest[0])
	}
	if len(specs) == 0 {
		fs.Usage()
		return fmt.Errorf("at least one -tunnel is required")
	}

	cfg, err := conn.resolve(specs)
	if err != nil {
		return err
	}
	return serve(cfg)
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("burrow login", flag.ExitOnError)
	server := fs.String("server", "", "tunnel server control endpoint, host:port")
	token := fs.String("token", "", "agent token (default: read from BURROW_TOKEN)")
	noTLS := fs.Bool("no-tls", false, "connect without TLS")
	insecure := fs.Bool("insecure", false, "skip server certificate verification")
	tlsName := fs.String("tls-name", "", "server name to verify in the certificate")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: burrow login -server host:port -token TOKEN\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	saved, err := client.LoadFileConfig()
	if err != nil {
		return err
	}
	// Only the tedious-to-retype fields fall back to the saved login. The
	// switches record exactly what was asked for: inheriting them meant a
	// -no-tls from an old plaintext server survived every later login and
	// could only be cleared with the non-obvious -no-tls=false, which showed
	// up as a bare "EOF" once the server grew a certificate.
	cfg := client.FileConfig{
		Server:   firstNonEmpty(*server, saved.Server, os.Getenv("BURROW_SERVER")),
		Token:    firstNonEmpty(*token, os.Getenv("BURROW_TOKEN"), saved.Token),
		NoTLS:    *noTLS,
		Insecure: *insecure,
		TLSName:  firstNonEmpty(*tlsName, saved.TLSName),
	}
	if cfg.Server == "" {
		return fmt.Errorf("-server is required")
	}
	if _, _, err := net.SplitHostPort(cfg.Server); err != nil {
		return fmt.Errorf("-server must be host:port, got %q", cfg.Server)
	}
	if cfg.Token == "" {
		return fmt.Errorf("-token is required (create one in the admin panel)")
	}

	path, err := client.SaveFileConfig(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("saved %s\n  server: %s\n  tls:    %v\n\nTry: burrow ssh\n",
		path, cfg.Server, !cfg.NoTLS)
	return nil
}

func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("burrow logout", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := client.DeleteFileConfig()
	if err != nil {
		return err
	}
	fmt.Printf("removed %s\n", path)
	return nil
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("burrow config", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := client.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := client.LoadFileConfig()
	if err != nil {
		return err
	}
	if cfg.Server == "" {
		fmt.Printf("no saved login (%s)\n\nRun: burrow login -server host:port -token TOKEN\n", path)
		return nil
	}
	fmt.Printf("%s\n  server: %s\n  token:  %s\n  tls:    %v\n",
		path, cfg.Server, maskToken(cfg.Token), !cfg.NoTLS)
	return nil
}

// warnIfNoSSHD tells the user up front when nothing is listening locally,
// which is a far more common mistake than a tunnel problem.
func warnIfNoSSHD(addr string) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		conn.Close()
		return
	}
	fmt.Fprintf(os.Stderr, "warning: nothing is listening on %s\n", addr)
	if runtime.GOOS == "darwin" {
		fmt.Fprintln(os.Stderr, "  enable it with: System Settings > General > Sharing > Remote Login")
	} else {
		fmt.Fprintln(os.Stderr, "  start it with: sudo systemctl start ssh")
	}
	fmt.Fprintln(os.Stderr, "  the tunnel will open anyway and start working once sshd is up")
}

// printGranted renders the live tunnel table after every (re)connect.
func printGranted(serverAddr string, granted []proto.TunnelResp) {
	name := "user"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nconnected to %s\n\n", serverAddr)
	for _, g := range granted {
		fmt.Fprintf(&b, "  %-4s  %s\n", g.Proto, g.URL)
		if g.Proto == proto.ProtoTCP {
			fmt.Fprintf(&b, "        ssh -p %d %s@%s\n", g.RemotePort, name, g.RemoteHost)
		}
	}
	fmt.Fprintln(&b, "\nctrl-c to stop")
	fmt.Print(b.String())
}

// specList collects repeated -tunnel flags.
type specList []client.TunnelSpec

func (s *specList) String() string { return fmt.Sprintf("%d tunnel(s)", len(*s)) }

func (s *specList) Set(v string) error {
	spec, err := client.ParseSpec(v)
	if err != nil {
		return err
	}
	*s = append(*s, spec)
	return nil
}

// parseInterspersed parses flags that appear before, between and after
// positional arguments. The stdlib flag package stops at the first non-flag
// word, which would silently ignore "burrow http 3000 -subdomain myapp";
// consuming one positional at a time and re-parsing the remainder fixes that.
func parseInterspersed(fs *flag.FlagSet, argv []string) ([]string, error) {
	var positional []string
	if err := fs.Parse(argv); err != nil {
		return nil, err
	}
	for {
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		if err := fs.Parse(rest[1:]); err != nil {
			return nil, err
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// maskToken shows just enough of a secret to recognise it.
func maskToken(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}

func usage() {
	fmt.Fprintf(os.Stderr, `burrow %s - tunnel agent

Usage:
  burrow login -server tun.example.com:7000 -token TOKEN
  burrow http <port>          publish a local HTTP service
  burrow tcp  <port>          publish a local TCP port
  burrow ssh                  publish local SSH, prints the ssh command
  burrow start -tunnel ...    several tunnels over one connection
  burrow config               show the saved login
  burrow logout               forget the saved login

Examples:
  burrow http 3000
  burrow http 3000 -subdomain myapp
  burrow ssh
  burrow ssh -port 25343
  burrow start -tunnel http:3000:myapp -tunnel tcp:22

Run "burrow <command> -h" for the flags of one command.
`, version)
}
