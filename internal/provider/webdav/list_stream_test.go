package webdav

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

const davStreamStart = `<d:multistatus xmlns:d="DAV:">`
const davStreamEnd = `</d:multistatus>`

func davStreamMember(name string) string {
	return fmt.Sprintf(`<d:response><d:href>/dav/%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:displayname>%s</d:displayname><d:getcontentlength>1</d:getcontentlength><d:getetag>v1</d:getetag></d:prop></d:propstat></d:response>`, name, name)
}

func TestListStreamDeliversBeforeHTTPBodyFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstVisited := make(chan struct{})
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" {
			t.Error("stream changed the PROPFIND contract")
		}
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, davStreamStart+davStreamMember("first"))
		w.(http.Flusher).Flush()
		select {
		case <-firstVisited:
		case <-r.Context().Done():
			return
		}
		for i := range 5000 {
			if _, err := io.WriteString(w, davStreamMember(fmt.Sprintf("file-%05d", i))); err != nil {
				return
			}
		}
		io.WriteString(w, davStreamEnd)
	}))
	defer srv.Close()
	p, err := New(Options{Name: "dav", BaseURL: srv.URL + "/dav", Client: httpx.New(httpx.Options{HTTP: srv.Client()})})
	if err != nil {
		t.Fatal(err)
	}
	stats := provider.NewStats()
	wrapped := provider.Instrument(p, stats)
	stream, ok := wrapped.(provider.StreamLister)
	if !ok || !wrapped.Capabilities().StreamList {
		t.Fatal("instrumentation dropped streaming capability")
	}
	count := 0
	err = stream.ListStream(ctx, "/", func(e provider.Entry) error {
		if count == 0 {
			if e.ID != "/first" || e.ParentID != "/" || e.Name != "first" {
				t.Fatalf("first entry: %+v", e)
			}
			close(firstVisited)
		}
		count++
		return nil
	})
	if err != nil || count != 5001 || requests.Load() != 1 || stats.Snapshot()["list"] != 1 {
		t.Fatalf("stream enumeration: count=%d requests=%d stats=%v err=%v", count, requests.Load(), stats.Snapshot(), err)
	}
}

func TestListStreamClosesBodyOnVisitorErrorOrCancellation(t *testing.T) {
	for _, cancelVisitor := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelVisitor), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			closed := make(chan struct{})
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusMultiStatus)
				io.WriteString(w, davStreamStart+davStreamMember("first"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(closed)
			}))
			defer srv.Close()
			p, err := New(Options{Name: "dav", BaseURL: srv.URL + "/dav", Client: httpx.New(httpx.Options{HTTP: srv.Client()})})
			if err != nil {
				t.Fatal(err)
			}
			visitorErr := errors.New("collector full")
			count := 0
			err = p.ListStream(ctx, "/", func(provider.Entry) error {
				count++
				if cancelVisitor {
					cancel()
					return nil
				}
				return visitorErr
			})
			want := visitorErr
			if cancelVisitor {
				want = context.Canceled
			}
			if !errors.Is(err, want) || count != 1 || requests.Load() != 1 {
				t.Fatalf("stream replayed/ignored visitor: count=%d requests=%d err=%v", count, requests.Load(), err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("response body left open")
			}
		})
	}
}

func TestStreamMultistatusRejectsIncompleteAndOversizedElements(t *testing.T) {
	for name, body := range map[string]string{
		"empty": "", "wrong-root": `<html/>`,
		"wrong-namespace": `<multistatus/>`,
		"truncated":       davStreamStart + davStreamMember("first"),
		"second-document": davStreamStart + davStreamEnd + `<extra/>`,
		"trailing-text":   davStreamStart + davStreamEnd + "garbage",
		"directive":       `<!DOCTYPE foo>` + davStreamStart + davStreamEnd,
		"huge-property":   davStreamStart + `<d:response><d:href>` + strings.Repeat("x", maxDAVElementBytes+8192) + `</d:href></d:response>` + davStreamEnd,
		"huge-extension":  davStreamStart + `<ignored>` + strings.Repeat("x", maxDAVElementBytes+8192) + `</ignored>` + davStreamEnd,
	} {
		t.Run(name, func(t *testing.T) {
			err := streamMultistatus(context.Background(), strings.NewReader(body), func(davResp) error { return nil })
			if err == nil {
				t.Fatal("malformed response accepted as a complete directory")
			}
			if strings.HasPrefix(name, "huge-") && !errors.Is(err, errDAVElementTooLarge) {
				t.Fatalf("oversized element was not budgeted: %v", err)
			}
		})
	}
}

func TestListStreamRejectsUnverifiableMembers(t *testing.T) {
	for name, member := range map[string]string{
		"failed-properties": strings.ReplaceAll(davStreamMember("bad"), "200 OK", "403 Forbidden"),
		"missing-href":      strings.ReplaceAll(davStreamMember("bad"), `<d:href>/dav/bad</d:href>`, ""),
		"wrong-parent":      davStreamMember("nested/bad"),
		"negative-size":     strings.ReplaceAll(davStreamMember("bad"), ">1<", ">-1<"),
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusMultiStatus)
				io.WriteString(w, davStreamStart+davStreamMember("good")+member+davStreamEnd)
			}))
			defer srv.Close()
			p, err := New(Options{Name: "dav", BaseURL: srv.URL + "/dav", Client: httpx.New(httpx.Options{HTTP: srv.Client()})})
			if err != nil {
				t.Fatal(err)
			}
			if entries, _, err := p.List(context.Background(), "/", ""); err == nil || len(entries) != 0 {
				t.Fatalf("compatibility List exposed incomplete collection: %+v %v", entries, err)
			}
		})
	}
}

func TestListStreamPreservesEscapedResourceIdentity(t *testing.T) {
	names := []string{"hash#name", "query?name", "literal%2Fname", "space 中文", "a&b"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, davStreamStart)
		for _, name := range names {
			fmt.Fprintf(w, `<d:response><d:href>/dav/%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:displayname>%s</d:displayname><d:getcontentlength>1</d:getcontentlength></d:prop></d:propstat></d:response>`, html.EscapeString(url.PathEscape(name)), html.EscapeString(name))
		}
		io.WriteString(w, davStreamEnd)
	}))
	defer srv.Close()
	p, err := New(Options{Name: "dav", BaseURL: srv.URL + "/dav", Client: httpx.New(httpx.Options{HTTP: srv.Client()})})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	err = p.ListStream(context.Background(), "/", func(e provider.Entry) error {
		if e.ID != "/"+names[count] || e.Name != names[count] {
			t.Errorf("escaped resource identity corrupted: %+v want=%q", e, names[count])
		}
		count++
		return nil
	})
	if err != nil || count != len(names) {
		t.Fatalf("escaped resources rejected: count=%d err=%v", count, err)
	}
}
