package webdav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// This bounds each response element (including ignored properties), not the
// complete directory. The XML decoder may additionally prefetch a small
// buffer. Excessive tokens/padding/extensions are rejected as well, rather
// than allowing one malformed property to consume directory-sized memory.
const maxDAVElementBytes = 1 << 20

var errDAVElementTooLarge = errors.New("webdav: PROPFIND element exceeds parsing budget")

// ListStream delivers each child as it arrives. HTTP retries may happen
// before a successful response body is opened; decoding and visitor failures
// never restart a body whose prefix has already been delivered.
func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	if visit == nil {
		return errors.New("webdav: nil directory visitor")
	}
	resp, err := p.client.Do(ctx, httpx.Request{
		Method: "PROPFIND", URL: p.urlFor(dirID), Class: ratelimit.Meta,
		Header: p.header(map[string]string{"Depth": "1", "Content-Type": "application/xml"}),
		Body:   strings.NewReader(propfindBody),
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(propfindBody)), nil
		},
		ExpectStatus: []int{http.StatusMultiStatus, http.StatusOK}, Stream: true,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	self := path.Clean("/" + strings.TrimPrefix(dirID, "/"))
	return streamMultistatus(ctx, resp.Body, func(r davResp) error {
		if r.Href == "" {
			return errors.New("webdav: PROPFIND response is missing href")
		}
		e, ok, err := p.entryFrom(r)
		if err != nil {
			return err
		}
		if !ok {
			// A member with failed properties is not evidence of absence.
			// Publishing the rest could remove its previously cached subtree.
			return errors.New("webdav: incomplete PROPFIND member properties")
		}
		if e.ID == self {
			return nil
		}
		if path.Dir(e.ID) != self || e.Size < 0 {
			return errors.New("webdav: invalid child in depth-one PROPFIND response")
		}
		e.ParentID = self
		return visit(e)
	})
}

type davBudgetReader struct {
	ctx  context.Context
	r    io.Reader
	left int64
}

func (r *davBudgetReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.left <= 0 {
		return 0, errDAVElementTooLarge
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	return n, err
}

func streamMultistatus(ctx context.Context, body io.Reader, visit func(davResp) error) error {
	r := &davBudgetReader{ctx: ctx, r: body}
	d := xml.NewDecoder(r)
	root := xml.Name{Space: "DAV:", Local: "multistatus"}
	member := xml.Name{Space: "DAV:", Local: "response"}
	started, finished := false, false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.left = maxDAVElementBytes
		token, err := d.Token()
		if err == io.EOF && finished {
			return nil
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("webdav: incomplete multistatus: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			if finished {
				return errors.New("webdav: trailing XML document")
			}
			if !started {
				if t.Name != root {
					return errors.New("webdav: expected DAV multistatus")
				}
				started = true
				continue
			}
			if t.Name != member {
				if err := d.Skip(); err != nil {
					return fmt.Errorf("webdav: invalid multistatus extension: %w", err)
				}
				continue
			}
			var response davResp
			if err := d.DecodeElement(&response, &t); err != nil {
				return fmt.Errorf("webdav: invalid PROPFIND member: %w", err)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(response); err != nil {
				return err
			}
		case xml.EndElement:
			if t.Name != root || !started || finished {
				return errors.New("webdav: unexpected multistatus close")
			}
			finished = true
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return errors.New("webdav: unexpected multistatus text")
			}
		case xml.Directive:
			return errors.New("webdav: XML directives are not supported")
		}
	}
}

var _ provider.StreamLister = (*Provider)(nil)
