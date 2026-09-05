// Package strmgen creates media-server .strm files by walking CloudFS's
// read-only WebDAV endpoint. It talks to the running owner instead of opening
// a second daemon or bypassing VFS through provider APIs.
package strmgen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const maxListingBytes = 8 << 20

const (
	manifestName     = ".cloudfs-strm-manifest.json"
	manifestVersion  = 1
	maxManifestBytes = 256 << 20
)

var defaultExtensions = []string{
	".3g2", ".3gp", ".asf", ".avi", ".flv", ".m2ts", ".m4v", ".mkv",
	".mov", ".mp4", ".mpeg", ".mpg", ".mts", ".rm", ".rmvb", ".ts", ".vob", ".webm", ".wmv",
}

type Options struct {
	Client         *http.Client
	StartURL       string
	OutputDir      string
	Token          string
	EmbedBasicAuth bool
	Prune          bool
	Extensions     []string
	MaxDepth       int
	MaxFiles       int
}

type Stats struct {
	Directories int
	Media       int
	Written     int
	Unchanged   int
	Skipped     int
	Pruned      int
	Retained    int
}

type generator struct {
	opt        Options
	root       *url.URL
	extensions map[string]bool
	seen       map[string]bool
	outputs    map[string]string
	manifest   manifest
	previous   manifest
	stats      Stats
}

type manifest struct {
	Version int               `json:"version"`
	Source  string            `json:"source"`
	Files   map[string]string `json:"files"`
}

func Generate(ctx context.Context, opt Options) (Stats, error) {
	if opt.Client == nil {
		opt.Client = http.DefaultClient
	}
	if opt.OutputDir == "" {
		return Stats{}, errors.New("strm: output directory is required")
	}
	if opt.EmbedBasicAuth && opt.Token == "" {
		return Stats{}, errors.New("strm: embedding Basic auth requires CLOUDFS_WEBDAV_TOKEN")
	}
	if opt.MaxDepth == 0 {
		opt.MaxDepth = 64
	}
	if opt.MaxFiles == 0 {
		opt.MaxFiles = 100000
	}
	if opt.MaxDepth < 0 || opt.MaxDepth > 256 || opt.MaxFiles < 1 || opt.MaxFiles > 1000000 {
		return Stats{}, errors.New("strm: max depth or file count is out of range")
	}
	root, err := url.Parse(opt.StartURL)
	if err != nil || (root.Scheme != "http" && root.Scheme != "https") || root.Host == "" || root.RawQuery != "" || root.Fragment != "" || root.User != nil {
		return Stats{}, errors.New("strm: start URL must be an http(s) URL without credentials, query, or fragment")
	}
	root.Path = strings.TrimSuffix(root.Path, "/") + "/"
	root.RawPath = ""
	exts := opt.Extensions
	if len(exts) == 0 {
		exts = defaultExtensions
	}
	g := &generator{
		opt: opt, root: root, extensions: map[string]bool{}, seen: map[string]bool{}, outputs: map[string]string{},
		manifest: manifest{Version: manifestVersion, Source: root.String(), Files: map[string]string{}},
	}
	for _, ext := range exts {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if strings.ContainsAny(ext, "/\\\x00") {
			return Stats{}, fmt.Errorf("strm: invalid extension %q", ext)
		}
		g.extensions[ext] = true
	}
	if len(g.extensions) == 0 {
		return Stats{}, errors.New("strm: no media extensions")
	}
	if err := ensureSafeDir(opt.OutputDir, ""); err != nil {
		return Stats{}, err
	}
	if opt.Prune {
		previous, err := loadManifest(opt.OutputDir, root.String())
		if err != nil {
			return Stats{}, err
		}
		g.previous = previous
	}
	if err := g.walk(ctx, root, "", 0); err != nil {
		return g.stats, err
	}
	if opt.Prune {
		if err := g.prune(); err != nil {
			return g.stats, err
		}
		body, err := json.MarshalIndent(g.manifest, "", "  ")
		if err != nil {
			return g.stats, err
		}
		body = append(body, '\n')
		if _, err := writeAtomic(opt.OutputDir, manifestName, body, 0o600); err != nil {
			return g.stats, fmt.Errorf("strm: write manifest: %w", err)
		}
	}
	return g.stats, nil
}

type multiStatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href      string        `xml:"href"`
	PropStats []davPropStat `xml:"propstat"`
}

type davPropStat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	ResourceType struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

type child struct {
	name  string
	url   *url.URL
	isDir bool
}

func (g *generator) walk(ctx context.Context, current *url.URL, relDir string, depth int) error {
	if depth > g.opt.MaxDepth {
		return fmt.Errorf("strm: directory depth exceeds %d at %s", g.opt.MaxDepth, current.Path)
	}
	key := current.String()
	if g.seen[key] {
		return fmt.Errorf("strm: WebDAV directory cycle at %s", current.Path)
	}
	g.seen[key] = true
	g.stats.Directories++

	children, err := g.list(ctx, current)
	if err != nil {
		return err
	}
	for _, c := range children {
		if c.isDir {
			if err := ensureSafeDir(g.opt.OutputDir, filepath.Join(relDir, c.name)); err != nil {
				return err
			}
			if err := g.walk(ctx, c.url, filepath.Join(relDir, c.name), depth+1); err != nil {
				return err
			}
			continue
		}
		ext := strings.ToLower(path.Ext(c.name))
		if !g.extensions[ext] {
			g.stats.Skipped++
			continue
		}
		g.stats.Media++
		if g.stats.Media > g.opt.MaxFiles {
			return fmt.Errorf("strm: media file count exceeds %d", g.opt.MaxFiles)
		}
		outName := strings.TrimSuffix(c.name, path.Ext(c.name)) + ".strm"
		outRel := filepath.Join(relDir, outName)
		outputKey := strings.ToLower(outRel)
		if previous, exists := g.outputs[outputKey]; exists {
			return fmt.Errorf("strm: %s and %s map to the same output %s", previous, c.name, outRel)
		}
		g.outputs[outputKey] = c.name
		streamURL := *c.url
		mode := os.FileMode(0o644)
		if g.opt.EmbedBasicAuth {
			if g.opt.Token == "" {
				return errors.New("strm: embedding Basic auth requires CLOUDFS_WEBDAV_TOKEN")
			}
			streamURL.User = url.UserPassword("cloudfs", g.opt.Token)
			mode = 0o600
		}
		content := []byte(streamURL.String() + "\n")
		manifestRel := filepath.ToSlash(outRel)
		g.manifest.Files[manifestRel] = contentDigest(content)
		changed, err := writeAtomic(g.opt.OutputDir, outRel, content, mode)
		if err != nil {
			return err
		}
		if changed {
			g.stats.Written++
		} else {
			g.stats.Unchanged++
		}
	}
	return nil
}

func contentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func loadManifest(root, source string) (manifest, error) {
	empty := manifest{Version: manifestVersion, Source: source, Files: map[string]string{}}
	target := filepath.Join(root, manifestName)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return manifest{}, fmt.Errorf("strm: inspect manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return manifest{}, errors.New("strm: manifest is not a regular file")
	}
	f, err := os.Open(target)
	if err != nil {
		return manifest{}, fmt.Errorf("strm: open manifest: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return manifest{}, fmt.Errorf("strm: read manifest: %w", readErr)
	}
	if closeErr != nil {
		return manifest{}, fmt.Errorf("strm: close manifest: %w", closeErr)
	}
	if len(body) > maxManifestBytes {
		return manifest{}, errors.New("strm: manifest is too large")
	}
	var old manifest
	if err := json.Unmarshal(body, &old); err != nil {
		return manifest{}, fmt.Errorf("strm: invalid manifest: %w", err)
	}
	if old.Version != manifestVersion || old.Source != source || old.Files == nil {
		return manifest{}, errors.New("strm: manifest version or source does not match; remove it manually to establish a new prune baseline")
	}
	for rel, digest := range old.Files {
		if !safeManifestPath(rel) || len(digest) != sha256.Size*2 {
			return manifest{}, fmt.Errorf("strm: unsafe manifest entry %q", rel)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return manifest{}, fmt.Errorf("strm: invalid manifest digest for %q", rel)
		}
	}
	return old, nil
}

func safeManifestPath(rel string) bool {
	if rel == "" || rel == manifestName || strings.Contains(rel, "\\") || path.Ext(rel) != ".strm" {
		return false
	}
	clean := path.Clean(rel)
	if clean != rel || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if !safeName(part) {
			return false
		}
	}
	return true
}

func (g *generator) prune() error {
	var stale []string
	for rel := range g.previous.Files {
		if _, current := g.manifest.Files[rel]; !current {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)
	dirtyDirs := map[string]bool{}
	for _, rel := range stale {
		target, exists, err := safeExistingFile(g.opt.OutputDir, rel)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		body, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("strm: read stale output %s: %w", rel, err)
		}
		if contentDigest(body) != g.previous.Files[rel] {
			g.stats.Retained++
			continue
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("strm: prune %s: %w", rel, err)
		}
		dirtyDirs[filepath.Dir(target)] = true
		g.stats.Pruned++
	}
	var dirs []string
	for dir := range dirtyDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if err := syncDirectory(dir); err != nil {
			return fmt.Errorf("strm: sync pruned directory %s: %w", dir, err)
		}
	}
	return nil
}

func safeExistingFile(root, rel string) (string, bool, error) {
	if !safeManifestPath(rel) {
		return "", false, fmt.Errorf("strm: unsafe stale output path %q", rel)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", false, err
	}
	if err := requireRealDir(abs); err != nil {
		return "", false, err
	}
	parts := strings.Split(rel, "/")
	cur := abs
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", false, fmt.Errorf("strm: stale output parent %s is not a real directory", cur)
		}
	}
	target := filepath.Join(cur, parts[len(parts)-1])
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("strm: stale output %s is not a regular file", rel)
	}
	return target, true, nil
}

func (g *generator) list(ctx context.Context, current *url.URL) ([]child, error) {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", current.String(), strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><prop><resourcetype/></prop></propfind>`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	if g.opt.Token != "" {
		req.SetBasicAuth("cloudfs", g.opt.Token)
	}
	resp, err := g.opt.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("strm: list %s: %w", current.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("strm: list %s returned %s", current.Path, resp.Status)
	}
	limited := io.LimitReader(resp.Body, maxListingBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxListingBytes {
		return nil, fmt.Errorf("strm: listing %s exceeds %d bytes", current.Path, maxListingBytes)
	}
	var listing multiStatus
	if err := xml.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("strm: invalid WebDAV listing for %s: %w", current.Path, err)
	}
	currentPath := strings.TrimSuffix(path.Clean(current.Path), "/")
	var out []child
	for _, response := range listing.Responses {
		if response.Href == "" || !successfulProp(response.PropStats) {
			continue
		}
		ref, err := url.Parse(response.Href)
		if err != nil {
			continue
		}
		resolved := current.ResolveReference(ref)
		if !sameOrigin(g.root, resolved) || !underRoot(g.root.Path, resolved.Path) {
			continue
		}
		childPath := strings.TrimSuffix(path.Clean(resolved.Path), "/")
		if childPath == currentPath {
			continue
		}
		if path.Dir(childPath) != currentPath {
			continue
		}
		name := path.Base(childPath)
		if !safeName(name) {
			continue
		}
		isDir := collection(response.PropStats)
		if isDir {
			resolved.Path = strings.TrimSuffix(resolved.Path, "/") + "/"
		}
		resolved.RawPath, resolved.RawQuery, resolved.Fragment, resolved.User = "", "", "", nil
		out = append(out, child{name: name, url: resolved, isDir: isDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func safeName(name string) bool {
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func successfulProp(stats []davPropStat) bool {
	for _, stat := range stats {
		if strings.Contains(stat.Status, " 200 ") {
			return true
		}
	}
	return false
}

func collection(stats []davPropStat) bool {
	for _, stat := range stats {
		if strings.Contains(stat.Status, " 200 ") && stat.Prop.ResourceType.Collection != nil {
			return true
		}
	}
	return false
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func underRoot(root, candidate string) bool {
	root = strings.TrimSuffix(path.Clean(root), "/")
	candidate = path.Clean(candidate)
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func ensureSafeDir(root, rel string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	if err := requireRealDir(abs); err != nil {
		return err
	}
	cur := abs
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if part == ".." || strings.ContainsAny(part, "\x00/\\") {
			return errors.New("strm: unsafe output path")
		}
		cur = filepath.Join(cur, part)
		if err := makeDir(cur); err != nil {
			return err
		}
	}
	return nil
}

func requireRealDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("strm: output path %s is not a real directory", dir)
	}
	return nil
}

func makeDir(dir string) error {
	_, err := os.Lstat(dir)
	if err == nil {
		return requireRealDir(dir)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	_, err = os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := requireRealDir(dir); err != nil {
		return fmt.Errorf("strm: output path %s did not become a real directory: %w", dir, err)
	}
	return nil
}

func writeAtomic(root, rel string, content []byte, mode os.FileMode) (bool, error) {
	dir := filepath.Join(root, filepath.Dir(rel))
	if err := ensureSafeDir(root, filepath.Dir(rel)); err != nil {
		return false, err
	}
	target := filepath.Join(root, rel)
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() {
		if existing, readErr := os.ReadFile(target); readErr == nil && string(existing) == string(content) {
			if info.Mode().Perm() != mode.Perm() {
				if err := os.Chmod(target, mode); err != nil {
					return false, err
				}
				f, err := os.Open(target)
				if err != nil {
					return false, err
				}
				err = f.Sync()
				closeErr := f.Close()
				if err != nil {
					return false, err
				}
				if closeErr != nil {
					return false, closeErr
				}
			}
			return false, nil
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".cloudfs-strm-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return false, err
	}
	if _, err := tmp.Write(content); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return false, err
	}
	ok = true
	if err := syncDirectory(dir); err != nil {
		return false, err
	}
	return true, nil
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
