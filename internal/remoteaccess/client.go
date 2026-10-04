package remoteaccess

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Client speaks the protocol from Go: an http.RoundTripper that turns an
// ordinary operator API request (http://anything/nodes) into a sealed call
// through the relay, and the sealed reply back into an ordinary response.
// harnessctl -remote uses it, so every harnessctl command works away from
// home with code the relay never served; the integration tests use it as
// the remote browser.
type Client struct {
	// BaseURL is the remote dashboard address, ending in a slash
	// (https://relay.example.com/r/hr1.../).
	BaseURL string
	Key     string
	// HTTP carries the outer requests to the relay (nil: a default one).
	HTTP *http.Client

	mu   sync.Mutex
	sess *clientSession
}

type clientSession struct {
	id       string
	c2s, s2c cipher.AEAD
	mu       sync.Mutex
	seq      uint64
	mode     string
}

// ErrSignedOut is a refusal of the session itself (expired, or the
// manager turned remote access off or changed the key).
var ErrSignedOut = errors.New("remoteaccess: signed out")

// Mode is the remote mode the manager reported at the last login.
func (c *Client) Mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return ""
	}
	return c.sess.mode
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Login runs the challenge and login, replacing any current session.
func (c *Client) Login(ctx context.Context) error {
	secret, err := NewSecret(c.Key)
	if err != nil {
		return err
	}
	var hello struct {
		Nonce string `json:"nonce"`
	}
	if err := c.postJSON(ctx, "hello", nil, &hello); err != nil {
		return err
	}
	cnBytes := make([]byte, 16)
	if _, err := rand.Read(cnBytes); err != nil {
		return err
	}
	cn := hex.EncodeToString(cnBytes)
	var login struct {
		Session     string `json:"session"`
		ServerProof string `json:"serverProof"`
		Mode        string `json:"mode"`
	}
	req := map[string]string{"nonce": hello.Nonce, "clientNonce": cn, "proof": hex.EncodeToString(secret.ClientProof(hello.Nonce, cn))}
	if err := c.postJSON(ctx, "login", req, &login); err != nil {
		return err
	}
	proof, err := hex.DecodeString(login.ServerProof)
	if err != nil || !Equal(proof, secret.ServerProof(hello.Nonce, cn)) {
		return errors.New("remoteaccess: the manager's reply could not be verified: this is not your manager")
	}
	c2s, s2c, err := secret.SessionKeys(hello.Nonce, cn)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sess = &clientSession{id: login.Session, c2s: c2s, s2c: s2c, mode: login.Mode}
	c.mu.Unlock()
	return nil
}

func (c *Client) postJSON(ctx context.Context, endpoint string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remoteaccess: %s: %s", resp.Status, plainError(data))
	}
	return json.Unmarshal(data, out)
}

// plainError extracts the message of a plain JSON refusal.
func plainError(data []byte) string {
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &v) == nil && v.Error != "" {
		return v.Error
	}
	return trimmed(data)
}

// RoundTrip seals req, sends it, and opens the reply. A session the
// manager no longer knows is replaced by a new login once, then retried:
// the manager refused it before running anything.
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, MaxBody+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > MaxBody {
			return nil, fmt.Errorf("remoteaccess: request body over %d MiB, too large to send remotely", MaxBody>>20)
		}
	}
	call := Request{Method: req.Method, Path: req.URL.RequestURI(), ContentType: req.Header.Get("Content-Type"), Body: body}
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		sess := c.sess
		c.mu.Unlock()
		if sess == nil {
			if err := c.Login(req.Context()); err != nil {
				return nil, err
			}
			continue
		}
		resp, err := c.call(req, sess, call)
		if errors.Is(err, ErrSignedOut) && attempt == 0 {
			c.mu.Lock()
			if c.sess == sess {
				c.sess = nil
			}
			c.mu.Unlock()
			continue
		}
		return resp, err
	}
}

func (c *Client) call(orig *http.Request, sess *clientSession, call Request) (*http.Response, error) {
	sess.mu.Lock()
	sess.seq++
	seq := sess.seq
	sess.mu.Unlock()
	sealed, err := SealRequest(sess.c2s, sess.id, seq, call)
	if err != nil {
		return nil, err
	}
	out, err := http.NewRequestWithContext(orig.Context(), http.MethodPost, c.BaseURL+"call", bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	out.Header.Set("Content-Type", "application/octet-stream")
	out.Header.Set(HeaderSession, sess.id)
	out.Header.Set(HeaderSeq, strconv.FormatUint(seq, 10))
	resp, err := c.http().Do(out)
	if err != nil {
		return nil, err
	}
	if resp.Header.Get(HeaderSealed) != "1" || resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: %s", ErrSignedOut, plainError(data))
		}
		return nil, fmt.Errorf("remoteaccess: %s: %s", resp.Status, plainError(data))
	}
	frames := NewFrameReader(resp.Body, sess.s2c, sess.id, seq)
	head, err := frames.ReadHead()
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	header := http.Header{}
	if head.Type != "" {
		header.Set("Content-Type", head.Type)
	}
	return &http.Response{
		Status:     strconv.Itoa(head.Status) + " " + http.StatusText(head.Status),
		StatusCode: head.Status,
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        header,
		Body:          readCloser{Reader: frames.Body(), Closer: resp.Body},
		ContentLength: -1,
		Request:       orig,
	}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Logout ends the session at the manager.
func (c *Client) Logout(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://manager"+LogoutPath, nil)
	if err != nil {
		return err
	}
	resp, err := c.RoundTrip(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	c.mu.Lock()
	c.sess = nil
	c.mu.Unlock()
	return nil
}

// LogoutPath is the one call the manager answers itself rather than
// passing to the operator API: it ends the calling session.
const LogoutPath = "/remote-session/logout"

// BaseURLOf normalizes a remote dashboard address to end in a slash.
func BaseURLOf(u string) string {
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}
