// Package auditship writes blocks of the audit trail somewhere the database
// cannot reach.
//
// The whole value of an off-host copy is that whoever can edit the database
// cannot edit the copy. That property lives in the deployment — a directory on
// a different host, mounted read-only from here, or an endpoint that only
// accepts appends — and not in this code. What this code can do is make the
// copy faithful, name it so a gap is visible, and refuse to report success for
// a write it did not confirm.
package auditship

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sink is where a block of audit entries goes.
//
// Put returns the reference by which the block can be read back — a path, an
// object key, a URL. Get reads it again, which is what makes verification
// possible; a sink that cannot read back can still ship, and Verify will say
// so rather than pretending the archive was checked.
type Sink interface {
	// Name identifies the destination in the shipment record.
	Name() string
	Put(ctx context.Context, artifact string, content []byte) (ref string, err error)
	Get(ctx context.Context, ref string) ([]byte, error)
}

// ErrNotReadable is returned by Get on a sink that only accepts writes.
var ErrNotReadable = fmt.Errorf("this destination cannot be read back from here")

// ---------------------------------------------------------------------------
// Directory
// ---------------------------------------------------------------------------

// DirSink writes each block as a file in a directory.
//
// The intended deployment is an NFS or SSHFS mount of a directory on another
// machine, where the account this process runs as may create files and may not
// rewrite them. Files are written 0400 and never opened for writing twice: a
// block that already exists is a duplicate shipment, and overwriting it would
// destroy the only copy of what was actually sent.
type DirSink struct {
	Dir string
}

// NewDirSink prepares a directory sink, creating the directory if it is
// missing.
func NewDirSink(dir string) (*DirSink, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("an audit archive directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("preparing the audit archive directory: %w", err)
	}
	return &DirSink{Dir: dir}, nil
}

func (s *DirSink) Name() string { return "dir:" + s.Dir }

func (s *DirSink) Put(_ context.Context, artifact string, content []byte) (string, error) {
	path := filepath.Join(s.Dir, artifact)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists in the archive; refusing to overwrite it", artifact)
	}

	// Written to a temporary name and renamed, so a reader never sees a half
	// file, and fsynced before the rename because a shipment recorded in the
	// database against bytes that the page cache lost is worse than no
	// shipment at all.
	tmp, err := os.CreateTemp(s.Dir, ".partial-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}

	// The directory entry itself has to be durable, or the rename can be lost.
	if dir, err := os.Open(s.Dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return artifact, nil
}

func (s *DirSink) Get(_ context.Context, ref string) ([]byte, error) {
	// The reference comes from the shipment row, but a path is a path: a row
	// naming ../../etc/passwd would read a file this process should not.
	if ref != filepath.Base(ref) {
		return nil, fmt.Errorf("archive reference %q is not a plain file name", ref)
	}
	return os.ReadFile(filepath.Join(s.Dir, ref))
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// HTTPSink posts each block to an endpoint that accepts appends.
//
// The signature is over the exact bytes, so the receiver can reject anything
// that did not come from this deployment; without it, an endpoint that accepts
// appends accepts them from anyone who finds the URL.
type HTTPSink struct {
	Endpoint string
	Secret   string
	Client   *http.Client
}

// NewHTTPSink prepares an endpoint sink.
func NewHTTPSink(endpoint, secret string, timeout time.Duration) (*HTTPSink, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, fmt.Errorf("an audit archive endpoint is required")
	}
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("an audit archive endpoint needs a signing secret; " +
			"an unsigned append endpoint accepts entries from anyone who finds it")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &HTTPSink{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Secret:   secret,
		Client:   &http.Client{Timeout: timeout},
	}, nil
}

func (s *HTTPSink) Name() string { return "http:" + s.Endpoint }

func (s *HTTPSink) Put(ctx context.Context, artifact string, content []byte) (string, error) {
	url := s.Endpoint + "/" + artifact
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(s.Secret))
	mac.Write(content)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Flowed-Signature", hex.EncodeToString(mac.Sum(nil)))

	resp, err := s.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	// 409 is how a receiver says it already has this block. That is not a
	// failure — a retry after a lost response is exactly how it happens — but
	// it must not be recorded as a fresh shipment either.
	if resp.StatusCode == http.StatusConflict {
		return artifact, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("the archive endpoint answered %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return artifact, nil
}

func (s *HTTPSink) Get(ctx context.Context, ref string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Endpoint+"/"+ref, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden {
		return nil, ErrNotReadable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the archive endpoint answered %d reading %s", resp.StatusCode, ref)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
