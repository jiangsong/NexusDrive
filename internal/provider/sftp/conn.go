package sftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"cloudfs/internal/provider"
)

// dialFunc opens a TCP connection. The daemon injects one that applies the
// proxy rules; without it the driver dials directly.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// conn owns the SSH transport and the SFTP session on top of it, and rebuilds
// both when the link drops.
//
// A long-lived mount outlives any single TCP connection: the server restarts,
// a laptop suspends, a NAT table forgets the flow. Every call therefore goes
// through do(), which reconnects once and retries when the failure looks like
// a dead session rather than a real error from the server.
type conn struct {
	addr    string
	cfg     *ssh.ClientConfig
	dial    dialFunc
	opts    []sftp.ClientOption
	keepAlv time.Duration
	// packet is the requested SFTP packet size; zero means choose from the
	// server's banner.
	packet int
	// onDrop runs when a session is torn down, so state that referenced it
	// (cached file handles) is forgotten.
	onDrop func()

	mu   sync.Mutex
	ssh  *ssh.Client
	cli  *sftp.Client
	gen  uint64 // bumped on every successful connect, so concurrent callers do not each redial
	stop chan struct{}
	once sync.Once
}

// pool spreads calls over several sessions. One SSH connection is one
// cipher stream on one server core: on a LAN that capped a cold sequential
// read near 235 MB/s with the link idle. Requests are dealt round-robin;
// each session reconnects on its own when its link drops.
type pool struct {
	sessions []*conn
	rr       atomic.Uint32
}

func (p *pool) do(ctx context.Context, fn func(*sftp.Client) error) error {
	s := p.sessions[int(p.rr.Add(1)-1)%len(p.sessions)]
	return s.do(ctx, fn)
}

// doFirst runs fn on the first session. Small reads go here: a random
// reader's next miss finds its handle warm on the same session, and one
// round trip is all such a read costs, so spreading it buys nothing and
// alternating sessions cost a third of the IOPS.
func (p *pool) doFirst(ctx context.Context, fn func(*sftp.Client) error) error {
	return p.sessions[0].do(ctx, fn)
}

func (p *pool) Close() error {
	for _, s := range p.sessions {
		s.Close()
	}
	return nil
}

func (c *conn) client(ctx context.Context) (*sftp.Client, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli != nil {
		return c.cli, c.gen, nil
	}
	if err := c.connectLocked(ctx); err != nil {
		return nil, 0, err
	}
	return c.cli, c.gen, nil
}

func (c *conn) connectLocked(ctx context.Context) error {
	netConn, err := c.dial(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %v", provider.ErrTransient, c.addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		netConn.SetDeadline(dl)
	}
	sc, chans, reqs, err := ssh.NewClientConn(netConn, c.addr, c.cfg)
	if err != nil {
		netConn.Close()
		if isAuthErr(err) {
			return fmt.Errorf("%w: ssh handshake with %s: %v", provider.ErrAuth, c.addr, err)
		}
		return fmt.Errorf("%w: ssh handshake with %s: %v", provider.ErrTransient, c.addr, err)
	}
	netConn.SetDeadline(time.Time{})
	sshClient := ssh.NewClient(sc, chans, reqs)
	opts := append([]sftp.ClientOption{packetOption(c.packet, sshClient.ServerVersion())}, c.opts...)
	cli, err := sftp.NewClient(sshClient, opts...)
	if err != nil {
		sshClient.Close()
		return fmt.Errorf("%w: start sftp subsystem on %s: %v", provider.ErrTransient, c.addr, err)
	}
	c.ssh, c.cli, c.gen = sshClient, cli, c.gen+1
	c.startKeepaliveLocked(sshClient)
	return nil
}

// startKeepaliveLocked pokes the server so an idle mount does not silently
// lose the connection to a NAT or firewall timeout.
func (c *conn) startKeepaliveLocked(client *ssh.Client) {
	if c.keepAlv <= 0 {
		return
	}
	stop := make(chan struct{})
	c.stop = stop
	go func() {
		t := time.NewTicker(c.keepAlv)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					return
				}
			}
		}
	}()
}

// dropLocked tears down a session known to be dead so the next call redials.
func (c *conn) dropLocked(gen uint64) {
	if c.gen != gen || c.cli == nil {
		return // someone else already reconnected
	}
	if c.stop != nil {
		close(c.stop)
		c.stop = nil
	}
	c.cli.Close()
	c.ssh.Close()
	c.cli, c.ssh = nil, nil
	if c.onDrop != nil {
		c.onDrop()
	}
}

// openSSHPacket is the largest read OpenSSH's sftp-server honours in one
// request (SFTP_MAX_READ_LENGTH); asking for more gets a short answer.
const openSSHPacket = 255 << 10

// packetOption picks the SFTP packet size for a new session. Bigger packets
// are fewer packets: at 32 KiB a 125 MB/s stream is four thousand packets a
// second through the SSH layer, and that overhead, not bandwidth, was the
// ceiling on a LAN. Only OpenSSH is known to honour more than the protocol
// minimum, so anything else stays at 32 KiB unless configured.
func packetOption(configured int, banner []byte) sftp.ClientOption {
	switch {
	case configured > 0:
		return sftp.MaxPacketUnchecked(configured)
	case strings.Contains(string(banner), "OpenSSH"):
		return sftp.MaxPacketUnchecked(openSSHPacket)
	default:
		return sftp.MaxPacket(32 << 10)
	}
}

// do runs fn against a live session, reconnecting once if the session turns
// out to be dead. fn must be safe to run twice.
func (c *conn) do(ctx context.Context, fn func(*sftp.Client) error) error {
	cli, gen, err := c.client(ctx)
	if err != nil {
		return err
	}
	err = fn(cli)
	if err == nil || !isConnDead(err) {
		return err
	}
	c.mu.Lock()
	c.dropLocked(gen)
	c.mu.Unlock()
	cli, _, err2 := c.client(ctx)
	if err2 != nil {
		return err2
	}
	return fn(cli)
}

func (c *conn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.stop != nil {
			close(c.stop)
			c.stop = nil
		}
		if c.cli != nil {
			c.cli.Close()
			c.ssh.Close()
			c.cli, c.ssh = nil, nil
			if c.onDrop != nil {
				c.onDrop()
			}
		}
	})
	return nil
}

// isConnDead reports whether the error means the session is gone, as opposed
// to the server refusing one operation.
func isConnDead(err error) bool {
	if err == nil {
		return false
	}
	var se *sftp.StatusError
	if errors.As(err, &se) {
		return false // the server answered, so the link is alive
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrPermission) {
		return false
	}
	s := err.Error()
	for _, m := range []string{"EOF", "use of closed network connection", "connection reset",
		"broken pipe", "connection lost", "session is closed", "client is closed"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	var ne net.Error
	return errors.As(err, &ne)
}

func isAuthErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unable to authenticate") ||
		strings.Contains(s, "no supported methods") ||
		strings.Contains(s, "permission denied")
}

// authMethods assembles the credentials to offer, in the order OpenSSH would.
func authMethods(o Options) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if o.Password != "" {
		methods = append(methods, ssh.Password(o.Password))
	}
	keyFiles := o.KeyFiles
	if len(keyFiles) == 0 && o.Password == "" {
		home, _ := os.UserHomeDir()
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			p := filepath.Join(home, ".ssh", n)
			if _, err := os.Stat(p); err == nil {
				keyFiles = append(keyFiles, p)
			}
		}
	}
	for _, kf := range keyFiles {
		b, err := os.ReadFile(kf)
		if err != nil {
			return nil, fmt.Errorf("sftp: read key %s: %w", kf, err)
		}
		var signer ssh.Signer
		if o.KeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(b, []byte(o.KeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(b)
		}
		if err != nil {
			return nil, fmt.Errorf("sftp: parse key %s: %w", kf, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" && o.UseAgent {
		if c, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(c).Signers))
		}
	}
	if len(methods) == 0 {
		return nil, errors.New("sftp: no usable credentials: set password, key_file, or run an ssh agent")
	}
	return methods, nil
}

// hostKeyCallback verifies the server against known_hosts unless the operator
// has explicitly opted out. Skipping verification on a link that may cross a
// proxy would leave the session open to interception, so it is never the
// default.
// It returns the callback to install and the raw known_hosts callback, which
// knownHostAlgos needs because the wrapper below replaces the typed KeyError.
func hostKeyCallback(o Options) (cb, raw ssh.HostKeyCallback, err error) {
	if o.InsecureHostKey {
		return ssh.InsecureIgnoreHostKey(), nil, nil
	}
	files := o.KnownHosts
	if len(files) == 0 {
		home, _ := os.UserHomeDir()
		p := filepath.Join(home, ".ssh", "known_hosts")
		if _, err := os.Stat(p); err != nil {
			return nil, nil, fmt.Errorf("sftp: no known_hosts at %s; connect once with ssh, "+
				"point known_hosts at a file, or set insecure_host_key: true", p)
		}
		files = append(files, p)
	}
	known, err := knownhosts.New(files...)
	if err != nil {
		return nil, nil, fmt.Errorf("sftp: known_hosts: %w", err)
	}
	// Wrap the callback so a mismatch says which of the two very different
	// situations it is: a host we have never seen, or a host whose key
	// changed under us.
	wrapped := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := known(hostname, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			if len(ke.Want) == 0 {
				return fmt.Errorf("sftp: %s is not in known_hosts; connect once with ssh, "+
					"or run ssh-keyscan %s >> ~/.ssh/known_hosts", hostname, hostname)
			}
			return fmt.Errorf("sftp: host key for %s does not match known_hosts. "+
				"Either the server was reinstalled or the connection is being intercepted; "+
				"verify the fingerprint before removing the old entry", hostname)
		}
		return err
	}
	return wrapped, known, nil
}

// knownHostAlgos returns the host key types known_hosts already holds for
// addr, most specific first.
//
// Without this the client advertises its own default preference, picks an
// algorithm the file has no entry for, and reports a key mismatch on a host
// that is in fact known. OpenSSH avoids this by ordering its proposal from the
// file; Go does not do it for us.
func knownHostAlgos(cb ssh.HostKeyCallback, addr string) []string {
	if cb == nil {
		return nil
	}
	pub, err := probeKey()
	if err != nil {
		return nil
	}
	err = cb(addr, &net.TCPAddr{IP: net.IPv4zero}, pub)
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) || len(ke.Want) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var algos []string
	for _, w := range ke.Want {
		t := w.Key.Type()
		if seen[t] {
			continue
		}
		seen[t] = true
		algos = append(algos, t)
		// An entry recorded as ssh-rsa also authorises the SHA-2 signature
		// algorithms over the same key.
		if t == ssh.KeyAlgoRSA {
			algos = append(algos, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512)
		}
	}
	return algos
}

// probeKey builds a throwaway public key used only to ask the known_hosts
// callback what it holds. It is never sent anywhere.
func probeKey() (ssh.PublicKey, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewPublicKey(pub)
}

func defaultUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}
