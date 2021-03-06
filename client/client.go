package chclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
	chshare "github.com/jpillora/chisel/share"
	"github.com/jpillora/chisel/share/ccrypto"
	"github.com/jpillora/chisel/share/cio"
	"github.com/jpillora/chisel/share/cnet"
	"github.com/jpillora/chisel/share/cos"
	"github.com/jpillora/chisel/share/settings"
	"github.com/jpillora/chisel/share/tunnel"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/errgroup"
)

//Config represents a client configuration
type Config struct {
	Fingerprint      string
	Auth             string
	KeepAlive        time.Duration
	MaxRetryCount    int
	MaxRetryInterval time.Duration
	Server           string
	Proxy            string
	Remotes          []string
	Headers          http.Header
	DialContext      func(ctx context.Context, network, addr string) (net.Conn, error)
}

//Client represents a client instance
type Client struct {
	*cio.Logger
	config    *Config
	computed  settings.Config
	sshConfig *ssh.ClientConfig
	proxyURL  *url.URL
	server    string
	connCount cnet.ConnCount
	stop      func()
	eg        *errgroup.Group
	tunnel    *tunnel.Tunnel
}

//NewClient creates a new client instance
func NewClient(c *Config) (*Client, error) {
	//apply default scheme
	if !strings.HasPrefix(c.Server, "http") {
		c.Server = "http://" + c.Server
	}
	if c.MaxRetryInterval < time.Second {
		c.MaxRetryInterval = 5 * time.Minute
	}
	u, err := url.Parse(c.Server)
	if err != nil {
		return nil, err
	}
	//apply default port
	if !regexp.MustCompile(`:\d+$`).MatchString(u.Host) {
		if u.Scheme == "https" || u.Scheme == "wss" {
			u.Host = u.Host + ":443"
		} else {
			u.Host = u.Host + ":80"
		}
	}
	//swap to websockets scheme
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	hasReverse := false
	hasSocks := false
	hasStdio := false
	client := &Client{
		Logger: cio.NewLogger("client"),
		config: c,
		computed: settings.Config{
			Version: chshare.BuildVersion,
		},
		server: u.String(),
	}
	for _, s := range c.Remotes {
		r, err := settings.DecodeRemote(s)
		if err != nil {
			return nil, fmt.Errorf("Failed to decode remote '%s': %s", s, err)
		}
		if r.Socks {
			hasSocks = true
		}
		if r.Reverse {
			hasReverse = true
		}
		if r.Stdio {
			if hasStdio {
				return nil, errors.New("Only one stdio is allowed")
			}
			hasStdio = true
		}
		client.computed.Remotes = append(client.computed.Remotes, r)
	}
	//set default log level
	client.Logger.Info = true
	//outbound proxy
	if p := c.Proxy; p != "" {
		client.proxyURL, err = url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("Invalid proxy URL (%s)", err)
		}
	}
	//ssh auth and config
	user, pass := settings.ParseAuth(c.Auth)
	client.sshConfig = &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		ClientVersion:   "SSH-" + chshare.ProtocolVersion + "-client",
		HostKeyCallback: client.verifyServer,
		Timeout:         30 * time.Second,
	}
	//prepare client tunnel
	client.tunnel = tunnel.New(tunnel.Config{
		Logger:   client.Logger,
		Inbound:  true, //client always accepts inbound
		Outbound: hasReverse,
		Socks:    hasReverse && hasSocks,
	})
	return client, nil
}

//Run starts client and blocks while connected
func (c *Client) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Start(ctx); err != nil {
		return err
	}
	return c.Wait()
}

func (c *Client) verifyServer(hostname string, remote net.Addr, key ssh.PublicKey) error {
	expect := c.config.Fingerprint
	got := ccrypto.FingerprintKey(key)
	if expect != "" && !strings.HasPrefix(got, expect) {
		return fmt.Errorf("Invalid fingerprint (%s)", got)
	}
	//overwrite with complete fingerprint
	c.Infof("Fingerprint %s", got)
	return nil
}

//Start client and does not block
func (c *Client) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.stop = cancel
	eg, ctx := errgroup.WithContext(ctx)
	c.eg = eg
	via := ""
	if c.proxyURL != nil {
		via = " via " + c.proxyURL.String()
	}
	c.Infof("Connecting to %s%s\n", c.server, via)
	//connect chisel server
	eg.Go(func() error {
		return c.connectionLoop(ctx)
	})
	//listen sockets
	eg.Go(func() error {
		clientInbound := c.computed.Remotes.Reversed(false)
		return c.tunnel.BindRemotes(ctx, clientInbound)
	})
	return nil
}

func (c *Client) connectionLoop(ctx context.Context) error {
	//connection loop!
	b := &backoff.Backoff{Max: c.config.MaxRetryInterval}
	for {
		connected, retry, err := c.connectionOnce(ctx)
		//reset backoff after successful connections
		if connected {
			b.Reset()
		}
		//connection error
		attempt := int(b.Attempt())
		maxAttempt := c.config.MaxRetryCount
		if err != nil {
			//show error and attempt counts
			msg := fmt.Sprintf("Connection error: %s", err)
			if attempt > 0 {
				msg += fmt.Sprintf(" (Attempt: %d", attempt)
				if maxAttempt > 0 {
					msg += fmt.Sprintf("/%d", maxAttempt)
				}
				msg += ")"
			}
			c.Debugf(msg)
		}
		//give up?
		if !retry || (maxAttempt >= 0 && attempt >= maxAttempt) {
			break
		}
		d := b.Duration()
		c.Infof("Retrying in %s...", d)
		select {
		case <-cos.AfterSignal(d):
			continue //retry now
		case <-ctx.Done():
			c.Infof("Cancelled")
			return nil
		}
	}
	c.Close()
	return nil
}

//connectionOnce connects to the chisel server and blocks
func (c *Client) connectionOnce(ctx context.Context) (connected, retry bool, err error) {
	//already closed?
	select {
	case <-ctx.Done():
		return false, false, io.EOF
	default:
		//still open
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	//prepare dialer
	d := websocket.Dialer{
		HandshakeTimeout: 45 * time.Second,
		Subprotocols:     []string{chshare.ProtocolVersion},
	}
	//optional proxy
	if p := c.proxyURL; p != nil {
		if err := c.setProxy(p, &d); err != nil {
			return false, false, err
		}
	}
	wsConn, _, err := d.DialContext(ctx, c.server, c.config.Headers)
	if err != nil {
		return false, true, err
	}
	conn := cnet.NewWebSocketConn(wsConn)
	// perform SSH handshake on net.Conn
	c.Debugf("Handshaking...")
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "", c.sshConfig)
	if err != nil {
		if strings.Contains(err.Error(), "unable to authenticate") {
			c.Infof("Authentication failed")
			c.Debugf(err.Error())
			retry = false
		} else if n, ok := err.(net.Error); ok && !n.Temporary() {
			c.Infof(err.Error())
			retry = false
		} else {
			c.Infof("retriable: %s", err.Error())
			retry = true
		}
		return false, retry, err
	}
	defer sshConn.Close()
	// chisel client handshake (reverse of server handshake)
	// send configuration
	c.Debugf("Sending config")
	t0 := time.Now()
	_, configerr, err := sshConn.SendRequest(
		"config",
		true,
		settings.EncodeConfig(c.computed),
	)
	if err != nil {
		c.Infof("Config verification failed")
		return false, false, err
	}
	if len(configerr) > 0 {
		return false, false, errors.New(string(configerr))
	}
	c.Infof("Connected (Latency %s)", time.Since(t0))
	//connected, handover ssh connection for tunnel to use, and block
	retry = true
	err = c.tunnel.BindSSH(ctx, sshConn, reqs, chans)
	if n, ok := err.(net.Error); ok && !n.Temporary() {
		retry = false
	}
	c.Infof("Disconnected")
	return true, retry, err
}

func (c *Client) setProxy(u *url.URL, d *websocket.Dialer) error {
	// CONNECT proxy
	if !strings.HasPrefix(u.Scheme, "socks") {
		d.Proxy = func(*http.Request) (*url.URL, error) {
			return u, nil
		}
		return nil
	}
	// SOCKS5 proxy
	if u.Scheme != "socks" && u.Scheme != "socks5h" {
		return fmt.Errorf(
			"unsupported socks proxy type: %s:// (only socks5h:// or socks:// is supported)",
			u.Scheme,
		)
	}
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{
			User:     u.User.Username(),
			Password: pass,
		}
	}
	socksDialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return err
	}
	d.NetDial = socksDialer.Dial
	return nil
}

//Wait blocks while the client is running.
func (c *Client) Wait() error {
	return c.eg.Wait()
}

//Close manually stops the client
func (c *Client) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return nil
}
