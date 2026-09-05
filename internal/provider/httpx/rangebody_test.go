package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const rangeFixture = "0123456789abcdefghijklmnopqrstuvwxyz"

// rangeServer answers with 206 and the correct window, or with 200 and the
// whole object, depending on honour.
func rangeServer(honour bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !honour {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, rangeFixture)
			return
		}
		var start, end int64
		if n, _ := parseRange(r.Header.Get("Range"), &start, &end); n == 0 {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, rangeFixture)
			return
		}
		if end >= int64(len(rangeFixture)) {
			end = int64(len(rangeFixture)) - 1
		}
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, rangeFixture[start:end+1])
	}))
}

// parseRange reads "bytes=start-end". It returns 2 when both bounds are
// present, which is the only form the tests generate.
func parseRange(v string, start, end *int64) (int, error) {
	if !strings.HasPrefix(v, "bytes=") {
		return 0, nil
	}
	return fmt.Sscanf(strings.TrimPrefix(v, "bytes="), "%d-%d", start, end)
}

func fetch(t *testing.T, url string, off, n int64) []byte {
	t.Helper()
	c := New(Options{})
	resp, err := c.Do(context.Background(), Request{
		Method:       http.MethodGet,
		URL:          url,
		Header:       http.Header{"Range": []string{RangeHeader(off, n)}},
		ExpectStatus: []int{http.StatusOK, http.StatusPartialContent},
		Stream:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := RangeBody(resp, off, n)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRangeBodyCompensatesForIgnoredRange is the property every driver depends
// on: a CDN that ignores Range must not be able to feed the block cache bytes
// from the wrong offset.
func TestRangeBodyCompensatesForIgnoredRange(t *testing.T) {
	honouring := rangeServer(true)
	defer honouring.Close()
	ignoring := rangeServer(false)
	defer ignoring.Close()

	cases := []struct {
		off, n int64
		want   string
	}{
		{0, 10, rangeFixture[0:10]},
		{10, 10, rangeFixture[10:20]},
		{30, 6, rangeFixture[30:36]},
		{5, 1, rangeFixture[5:6]},
	}
	for _, c := range cases {
		if got := string(fetch(t, honouring.URL, c.off, c.n)); got != c.want {
			t.Errorf("honouring server, range %d+%d = %q, want %q", c.off, c.n, got, c.want)
		}
		if got := string(fetch(t, ignoring.URL, c.off, c.n)); got != c.want {
			t.Errorf("ignoring server, range %d+%d = %q, want %q", c.off, c.n, got, c.want)
		}
	}
}

func TestRangeBodyOpenEndedRead(t *testing.T) {
	ignoring := rangeServer(false)
	defer ignoring.Close()
	// n <= 0 means "to the end", so the tail from off must come back whole.
	got := string(fetch(t, ignoring.URL, 30, 0))
	if got != rangeFixture[30:] {
		t.Fatalf("open-ended read = %q, want %q", got, rangeFixture[30:])
	}
}

func TestRangeBodyReportsShortObject(t *testing.T) {
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "tiny")
	}))
	defer short.Close()

	c := New(Options{})
	resp, err := c.Do(context.Background(), Request{
		Method: http.MethodGet, URL: short.URL,
		Header:       http.Header{"Range": []string{RangeHeader(100, 10)}},
		ExpectStatus: []int{http.StatusOK, http.StatusPartialContent},
		Stream:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Asking past the end of an object the server sent whole must fail loudly
	// rather than return an empty read that looks like a hole in the file.
	if _, err := RangeBody(resp, 100, 10); err == nil {
		t.Fatal("expected an error when the object is shorter than the offset")
	} else if !strings.Contains(err.Error(), "shorter than offset") {
		t.Fatalf("error should explain the cause: %v", err)
	}
}

func TestRangeBodyNilResponse(t *testing.T) {
	if _, err := RangeBody(nil, 0, 10); err == nil {
		t.Fatal("a nil response should error rather than panic")
	}
	if _, err := RangeBody(&Response{}, 0, 10); err == nil {
		t.Fatal("a response with no body should error rather than panic")
	}
}
