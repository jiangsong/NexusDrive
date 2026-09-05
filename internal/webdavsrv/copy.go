package webdavsrv

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"cloudfs/internal/vfs"
)

// serverSideCopy handles COPY itself instead of letting the DAV handler stream
// the bytes.
//
// x/net/webdav implements COPY by opening the source, opening the destination
// and copying between them. On a cloud drive that means downloading the whole
// file and uploading it again — for a copy within one account, where the
// backend can do it server-side, and for a cross-account copy, where the VFS
// can hand the target an already-cached blob for a hash-only upload. Routing
// the verb to vfs.Copy is what makes the capability that already exists in the
// VFS reachable from a DAV client.
//
// Collections are refused: vfs.Copy is a file primitive by design, and walking
// a tree here would put a recursive operation in an adapter.
func serverSideCopy(next http.Handler, files *davFS, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "COPY" || files.writer == nil {
			next.ServeHTTP(w, r)
			return
		}
		status, err := copyResource(files, prefix, w, r)
		if err != nil || status >= 400 {
			message := "copy failed"
			if err != nil {
				message = err.Error()
			}
			http.Error(w, message, status)
			return
		}
		w.WriteHeader(status)
	})
}

func copyResource(files *davFS, prefix string, w http.ResponseWriter, r *http.Request) (int, error) {
	ctx := r.Context()
	srcName, ok := stripPrefix(r.URL.Path, prefix)
	if !ok {
		return http.StatusNotFound, errors.New("path is outside the DAV namespace")
	}
	dstName, status, err := destination(r, prefix)
	if err != nil {
		return status, err
	}
	srcVirtual, err := files.resolve(srcName)
	if err != nil {
		return http.StatusBadRequest, err
	}
	dstVirtual, err := files.resolve(dstName)
	if err != nil {
		return http.StatusBadRequest, err
	}
	if srcVirtual == dstVirtual {
		return http.StatusForbidden, errors.New("source and destination are the same resource")
	}
	srcAttr, err := files.backend.StatPath(ctx, srcVirtual)
	if err != nil {
		if errors.Is(mapError(err), fs.ErrNotExist) {
			return http.StatusNotFound, errors.New("source does not exist")
		}
		return http.StatusForbidden, err
	}
	if srcAttr.IsDir {
		// Say so plainly rather than half-copying a tree: a client that gets a
		// 403 here can fall back to walking the collection itself.
		return http.StatusForbidden, errors.New("copying a collection is not supported; copy its members individually")
	}

	overwrite := !strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "F")
	replaced := false
	if _, err := files.backend.StatPath(ctx, dstVirtual); err == nil {
		if !overwrite {
			return http.StatusPreconditionFailed, errors.New("destination exists and Overwrite is F")
		}
		if err := files.RemoveAll(ctx, dstName); err != nil {
			return http.StatusForbidden, err
		}
		replaced = true
	} else if !errors.Is(mapError(err), fs.ErrNotExist) {
		return http.StatusForbidden, err
	}

	if _, err := files.writer.Copy(ctx, srcVirtual, dstVirtual); err != nil {
		switch {
		case errors.Is(err, vfs.ErrNotFound):
			return http.StatusConflict, errors.New("destination collection does not exist")
		case errors.Is(err, vfs.ErrExists):
			return http.StatusPreconditionFailed, errors.New("destination already exists")
		case errors.Is(err, vfs.ErrReadOnly):
			return http.StatusForbidden, errors.New("destination is on a read-only mount")
		case errors.Is(err, vfs.ErrIsDir), errors.Is(err, vfs.ErrNotDir):
			return http.StatusForbidden, err
		default:
			return http.StatusBadGateway, err
		}
	}
	if replaced {
		return http.StatusNoContent, nil
	}
	return http.StatusCreated, nil
}

// destination parses the Destination header. A destination on another host is
// a cross-server copy, which this endpoint does not perform; RFC 4918 gives it
// 502 rather than a generic failure.
func destination(r *http.Request, prefix string) (string, int, error) {
	raw := r.Header.Get("Destination")
	if raw == "" || len(raw) > 8192 || strings.ContainsAny(raw, "\x00\r\n") {
		return "", http.StatusBadRequest, errors.New("Destination header is missing or malformed")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", http.StatusBadRequest, errors.New("Destination header is not a valid URI")
	}
	if u.Host != "" && !strings.EqualFold(u.Host, r.Host) {
		return "", http.StatusBadGateway, errors.New("cross-server copy is not supported")
	}
	name, ok := stripPrefix(u.Path, prefix)
	if !ok {
		return "", http.StatusForbidden, errors.New("destination is outside the DAV namespace")
	}
	return name, http.StatusOK, nil
}

// stripPrefix removes the DAV prefix, rejecting a path that only shares its
// leading characters ("/davish" against a "/dav" prefix).
func stripPrefix(p, prefix string) (string, bool) {
	if p == prefix {
		return "/", true
	}
	if strings.HasPrefix(p, prefix+"/") {
		return strings.TrimPrefix(p, prefix), true
	}
	return "", false
}
