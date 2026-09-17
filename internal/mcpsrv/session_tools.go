package mcpsrv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The session tools give an agent a delivery box: begin_session creates a
// directory of its own under the workspace and writes a manifest skeleton,
// finish_session fills the manifest with what the audit trail says the
// session wrote, and list_sessions shows the principal its own sessions.
// They only exist with a session store, and only the owner of the cache
// runs them: a stdio server beside a running mount has a VFS of its own and
// would leave the manifest in a journal nobody uploads.

// artifactAuditRows bounds how many audit rows finish_session reads back
// to derive the artifact list.
const artifactAuditRows = 10000

// shareLinkTimeout bounds each download-link request finish_session makes
// on behalf of share=true, so a slow provider cannot hold the tool.
const shareLinkTimeout = time.Second

// maxHashBytes bounds the file size finish_session will read back to
// record a sha256. Larger files get no checksum rather than a long read.
const maxHashBytes = 32 << 20

// errSessionNotYours is the refusal for finishing a session another
// principal opened.
var errSessionNotYours = errors.New("session belongs to another principal")

type beginSessionInput struct {
	Name    string `json:"name,omitempty" jsonschema:"Short label for this task"`
	Sandbox bool   `json:"sandbox,omitempty" jsonschema:"Limit writes to this session's directory"`
}

type beginSessionOutput struct {
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace"`
	URI       string `json:"uri"`
}

type finishSessionInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"Session to finish; default the current one"`
	Summary   string `json:"summary,omitempty" jsonschema:"What the session did, kept in the manifest"`
	Share     bool   `json:"share,omitempty" jsonschema:"Attach download links for files already uploaded"`
}

type finishSessionOutput struct {
	SessionID string           `json:"session_id"`
	Manifest  string           `json:"manifest,omitempty"`
	Artifacts []agent.Artifact `json:"artifacts"`
}

type listSessionsInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from a previous truncated listing"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum sessions to return; the server caps this"`
	State  string `json:"state,omitempty" jsonschema:"active, finished, expired or rolled_back; default all"`
}

type listSessionsOutput struct {
	Sessions   []agent.Session `json:"sessions"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func (s *Server) registerSessionTools() {
	if s.opt.Sessions == nil {
		return
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "begin_session",
		Description: "Start a delivery session: creates a directory of your own under the workspace and returns it. Write your deliverables there with the ordinary write tools; sandbox=true refuses writes anywhere else.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, s.beginSession)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "finish_session",
		Description: "End a delivery session: records the files it wrote as artifacts in its manifest.json, with a summary. share=true adds download links for files already uploaded.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, s.finishSession)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List your sessions, newest first, with their workspaces and artifacts.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.listSessions)
}

func (s *Server) beginSession(ctx context.Context, _ *mcp.CallToolRequest, in beginSessionInput) (*mcp.CallToolResult, beginSessionOutput, error) {
	if s.opt.NonOwner {
		r, _ := fail(errRequiresOwner)
		return r, beginSessionOutput{}, nil
	}
	cur, ok := agent.FromContext(ctx)
	if !ok {
		r, _ := fail(errors.New("no session on this connection"))
		return r, beginSessionOutput{}, nil
	}
	// The workspace is checked against the principal's scope, not the
	// current session's: a sandboxed session may begin another one, and
	// the sandbox is the one thing about the scope a session adds.
	base := cur.Scope
	base.Sandbox = ""
	ws, err := agent.DefaultWorkspace(s.opt.Workspace, base)
	if err != nil {
		r, _ := fail(err)
		return r, beginSessionOutput{}, nil
	}
	if _, err := base.Check(ws, true); err != nil {
		recordCheck(ctx, ws, err)
		r, _ := fail(fmt.Errorf("workspace %s: %w", ws, err))
		return r, beginSessionOutput{}, nil
	}
	recordCheck(ctx, ws, nil)
	sess, err := s.opt.Sessions.Begin(ctx, cur, agent.BeginOptions{Name: in.Name, Sandbox: in.Sandbox, Workspace: ws})
	if err != nil {
		r, _ := fail(err)
		return r, beginSessionOutput{}, nil
	}
	if err := s.createSessionDir(ctx, sess); err != nil {
		// The row exists but its directory does not; close it so the next
		// call starts over rather than writing into a directory that was
		// never made.
		_, _ = s.opt.Sessions.FinishWith(context.WithoutCancel(ctx), sess.ID, "", nil)
		r, _ := fail(mapErr(err, sess.Workspace))
		return r, beginSessionOutput{}, nil
	}
	out := beginSessionOutput{SessionID: sess.ID, Workspace: sess.Workspace, URI: s.uriOf(sess.Workspace)}
	return text("session %s: write your deliverables under %s", sess.ID, sess.Workspace), out, nil
}

// createSessionDir makes the session directory and writes the manifest
// skeleton. A directory that already exists is an error: session
// directories are never reused.
func (s *Server) createSessionDir(ctx context.Context, sess agent.Session) error {
	if _, err := s.opt.FS.StatPath(ctx, sess.Workspace); err == nil {
		return fmt.Errorf("%s already exists", sess.Workspace)
	} else if !errors.Is(err, vfs.ErrNotFound) {
		return err
	}
	if _, err := s.mkdirAll(ctx, sess.Workspace, false); err != nil {
		return err
	}
	return s.writeManifest(ctx, sess)
}

// writeManifest writes (or overwrites) the session's manifest.json.
func (s *Server) writeManifest(ctx context.Context, sess agent.Session) error {
	body, err := agent.ManifestOf(sess, s.principalLabel(ctx, sess.PrincipalID)).JSON()
	if err != nil {
		return err
	}
	_, err = s.opt.FS.WriteFile(ctx, agent.ManifestPath(sess.Workspace), body, false)
	return err
}

// principalLabel is the "<kind>:<name>" a manifest names as its principal.
func (s *Server) principalLabel(ctx context.Context, id string) string {
	p, err := s.opt.Sessions.Principal(ctx, id)
	if err != nil {
		return id
	}
	return p.Kind + ":" + p.Name
}

// uriOf is the cloudfs:// resource URI of a mount path.
func (s *Server) uriOf(p string) string {
	m, ok := s.resourceMount(p)
	if !ok {
		return ""
	}
	return resourceURI(m.Remote, p)
}

func (s *Server) finishSession(ctx context.Context, _ *mcp.CallToolRequest, in finishSessionInput) (*mcp.CallToolResult, finishSessionOutput, error) {
	if s.opt.NonOwner {
		r, _ := fail(errRequiresOwner)
		return r, finishSessionOutput{}, nil
	}
	cur, ok := agent.FromContext(ctx)
	if !ok {
		r, _ := fail(errors.New("no session on this connection"))
		return r, finishSessionOutput{}, nil
	}
	id := in.SessionID
	if id == "" {
		id = cur.ID
	}
	sess, err := s.opt.Sessions.Get(ctx, id)
	if err != nil {
		r, _ := fail(err)
		return r, finishSessionOutput{}, nil
	}
	if sess.PrincipalID != cur.PrincipalID {
		r, _ := fail(errSessionNotYours)
		return r, finishSessionOutput{}, nil
	}
	if sess.State != "active" {
		r, _ := fail(fmt.Errorf("session %s is already %s", id, sess.State))
		return r, finishSessionOutput{}, nil
	}
	rows, err := s.opt.Sessions.Store().AuditForSession(ctx, id, artifactAuditRows)
	if err != nil {
		r, _ := fail(err)
		return r, finishSessionOutput{}, nil
	}
	arts := s.collectArtifacts(ctx, agent.ArtifactPaths(rows, sess.Workspace), in.Share)
	// The row is closed first: agent.db is the record the console reads,
	// and the manifest is its copy on the mount, written with the same
	// finishing time and summary the row carries.
	done, err := s.opt.Sessions.FinishWith(ctx, id, in.Summary, arts)
	if err != nil {
		r, _ := fail(err)
		return r, finishSessionOutput{}, nil
	}
	out := finishSessionOutput{SessionID: id, Artifacts: arts}
	if done.Workspace != "" {
		if err := s.writeManifest(ctx, done); err != nil {
			r, _ := fail(fmt.Errorf("session finished but its manifest was not written: %w", mapErr(err, agent.ManifestPath(done.Workspace))))
			return r, finishSessionOutput{}, nil
		}
		out.Manifest = agent.ManifestPath(done.Workspace)
	}
	msg := fmt.Sprintf("session %s finished with %d artifacts", id, len(arts))
	if out.Manifest != "" {
		msg += " (manifest " + out.Manifest + ")"
	}
	return text("%s", msg), out, nil
}

// collectArtifacts describes each path that still exists as a file. A file
// waiting in the upload queue is "local" and never asked for a link: the
// provider has nothing to link to yet, and waiting for the upload is what
// flush_uploads is for. With share, a synced file gets a download link when
// the provider hands one out within shareLinkTimeout.
func (s *Server) collectArtifacts(ctx context.Context, paths []string, share bool) []agent.Artifact {
	arts := []agent.Artifact{}
	for _, p := range paths {
		attr, err := s.opt.FS.StatPath(ctx, p)
		if err != nil || attr.IsDir {
			continue
		}
		a := agent.Artifact{Path: p, URI: s.uriOf(p), Size: attr.Size, State: "synced", ConsoleURL: agent.ConsoleURL(s.opt.ConsoleURL, p)}
		if attr.LocalOnly {
			a.State = "local"
		}
		a.SHA256 = s.hashIfCached(ctx, p, attr)
		if share && a.State == "synced" {
			lctx, cancel := context.WithTimeout(ctx, shareLinkTimeout)
			link, err := s.opt.FS.DownloadURL(lctx, p)
			cancel()
			if err == nil {
				a.DownloadURL, a.ExpiresAt = link.URL, link.ExpiresAt
			}
		}
		arts = append(arts, a)
	}
	return arts
}

// hashIfCached returns the sha256 of a file whose bytes are all on disk and
// not too large; anything else costs provider traffic and gets "".
func (s *Server) hashIfCached(ctx context.Context, p string, attr vfs.Attr) string {
	if attr.Size > maxHashBytes || (attr.Cached < 1 && !attr.LocalOnly) {
		return ""
	}
	data, err := s.opt.FS.ReadFileRange(ctx, p, 0, 0)
	if err != nil || int64(len(data)) != attr.Size {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Server) listSessions(ctx context.Context, _ *mcp.CallToolRequest, in listSessionsInput) (*mcp.CallToolResult, listSessionsOutput, error) {
	if s.opt.NonOwner {
		r, _ := fail(errRequiresOwner)
		return r, listSessionsOutput{}, nil
	}
	cur, ok := agent.FromContext(ctx)
	if !ok {
		r, _ := fail(errors.New("no session on this connection"))
		return r, listSessionsOutput{}, nil
	}
	switch in.State {
	case "", "active", "finished", "expired", "rolled_back":
	default:
		r, _ := fail(fmt.Errorf("unknown state %q; use active, finished, expired or rolled_back", in.State))
		return r, listSessionsOutput{}, nil
	}
	sessions, next, err := s.opt.Sessions.List(ctx, agent.ListQuery{
		Cursor: in.Cursor, Limit: in.Limit, State: in.State, PrincipalID: cur.PrincipalID,
	})
	if err != nil {
		r, _ := fail(err)
		return r, listSessionsOutput{}, nil
	}
	out := listSessionsOutput{Sessions: sessions, NextCursor: next}
	msg := fmt.Sprintf("%d sessions", len(sessions))
	if next != "" {
		msg += fmt.Sprintf(" (pass cursor %q for more)", next)
	}
	return text("%s", msg), out, nil
}
