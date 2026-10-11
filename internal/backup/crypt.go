package backup

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// An encrypted backup is a small header followed by the zip, sealed in
// 64 KiB chunks with AES-256-GCM under a key stretched from the passphrase
// (PBKDF2-HMAC-SHA256; the standard library has no scrypt or argon2).
// Chunking keeps memory flat for a big artifact store. Each chunk's nonce
// is a random per-file prefix plus its position, and the header plus a
// "last chunk" flag are authenticated with it, so reordering, cutting the
// file short, or editing the header all fail to open.
//
//	"HHBKENC1" | iterations u32 | salt 16 | nonce prefix 4 | chunk...
const (
	encMagic     = "HHBKENC1"
	encChunk     = 64 << 10
	encHeaderLen = len(encMagic) + 4 + 16 + 4

	// DefaultIterations is OWASP's figure for PBKDF2-HMAC-SHA256.
	DefaultIterations = 600_000
	maxIterations     = 20_000_000
)

// ErrBadPassphrase means the first chunk would not open: the passphrase is
// wrong, or the file is not a backup.
var ErrBadPassphrase = errors.New("backup: wrong passphrase, or the file is damaged")

// ErrEncrypted means a passphrase is needed.
var ErrEncrypted = errors.New("backup: this backup is encrypted; give the passphrase")

// IsEncrypted reports whether the first bytes of a file are an encrypted
// backup's header.
func IsEncrypted(head []byte) bool {
	return len(head) >= len(encMagic) && string(head[:len(encMagic)]) == encMagic
}

func deriveKey(pass string, salt []byte, iter int) (cipher.AEAD, error) {
	k, err := pbkdf2.Key(sha256.New, pass, salt, iter, 32)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func chunkNonce(prefix []byte, n uint64) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[4:], n)
	return nonce
}

func chunkAAD(header []byte, last bool) []byte {
	aad := append(append([]byte{}, header...), 0)
	if last {
		aad[len(aad)-1] = 1
	}
	return aad
}

type encWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	header []byte
	prefix []byte
	buf    []byte // held back until we know whether it is the last chunk
	n      uint64
	err    error
}

// NewEncryptWriter wraps w. Close must be called to write the last chunk.
// iterations <= 0 means DefaultIterations.
func NewEncryptWriter(w io.Writer, passphrase string, iterations int) (io.WriteCloser, error) {
	if passphrase == "" {
		return nil, errors.New("backup: empty passphrase")
	}
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	header := make([]byte, encHeaderLen)
	copy(header, encMagic)
	binary.BigEndian.PutUint32(header[len(encMagic):], uint32(iterations))
	if _, err := rand.Read(header[len(encMagic)+4:]); err != nil {
		return nil, err
	}
	aead, err := deriveKey(passphrase, header[len(encMagic)+4:len(encMagic)+20], iterations)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(header); err != nil {
		return nil, err
	}
	return &encWriter{w: w, aead: aead, header: header, prefix: header[len(header)-4:]}, nil
}

func (e *encWriter) seal(last bool) {
	if e.err != nil {
		return
	}
	out := e.aead.Seal(nil, chunkNonce(e.prefix, e.n), e.buf, chunkAAD(e.header, last))
	e.n++
	e.buf = e.buf[:0]
	_, e.err = e.w.Write(out)
}

func (e *encWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 && e.err == nil {
		if len(e.buf) == encChunk { // more data follows: the held chunk is not the last
			e.seal(false)
		}
		n := min(encChunk-len(e.buf), len(p))
		e.buf = append(e.buf, p[:n]...)
		p = p[n:]
	}
	return total, e.err
}

func (e *encWriter) Close() error {
	e.seal(true)
	return e.err
}

type decReader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	header []byte
	prefix []byte
	n      uint64
	out    []byte
	done   bool
}

// NewDecryptReader reads an encrypted backup from r.
func NewDecryptReader(r io.Reader, passphrase string) (io.Reader, error) {
	br := bufio.NewReaderSize(r, encChunk+64)
	header := make([]byte, encHeaderLen)
	if _, err := io.ReadFull(br, header); err != nil || !IsEncrypted(header) {
		return nil, errors.New("backup: not an encrypted backup")
	}
	iter := int(binary.BigEndian.Uint32(header[len(encMagic):]))
	if iter < 1 || iter > maxIterations {
		return nil, fmt.Errorf("backup: unreasonable key-stretching count %d in the header", iter)
	}
	if passphrase == "" {
		return nil, ErrEncrypted
	}
	aead, err := deriveKey(passphrase, header[len(encMagic)+4:len(encMagic)+20], iter)
	if err != nil {
		return nil, err
	}
	return &decReader{r: br, aead: aead, header: header, prefix: header[len(header)-4:]}, nil
}

func (d *decReader) next() error {
	buf := make([]byte, encChunk+d.aead.Overhead())
	n, err := io.ReadFull(d.r, buf)
	last := false
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		last = true
		buf = buf[:n]
	case err != nil:
		return err
	default:
		if _, perr := d.r.Peek(1); perr == io.EOF {
			last = true
		}
	}
	plain, oerr := d.aead.Open(nil, chunkNonce(d.prefix, d.n), buf, chunkAAD(d.header, last))
	if oerr != nil {
		if d.n == 0 {
			return ErrBadPassphrase
		}
		return errors.New("backup: the file is damaged or was cut short (decryption failed)")
	}
	d.n++
	d.out = plain
	d.done = last
	return nil
}

func (d *decReader) Read(p []byte) (int, error) {
	for len(d.out) == 0 {
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}
