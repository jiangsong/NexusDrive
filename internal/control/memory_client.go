package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"cloudfs/internal/memory"
)

// The CallMemory* helpers are `cloudfs memory`'s side of the routes above.
// There is no offline half: the facts live in the daemon's VFS, so every
// call reports online=false when no daemon answers and the CLI says so.

// memoryTarget builds /memory/<agent>[/<name>] with each segment escaped;
// the server re-validates both.
func memoryTarget(agent, name string) string {
	t := "/memory/" + url.PathEscape(agent)
	if name != "" {
		t += "/" + url.PathEscape(name)
	}
	return t
}

// CallMemoryAgents asks the running daemon for the agent table.
func CallMemoryAgents(ctx context.Context, socket, tcp string) (MemoryAgentsResponse, bool, error) {
	var out MemoryAgentsResponse
	online, err := callControl(ctx, socket, tcp, http.MethodGet, "/memory/agents", nil, &out)
	return out, online, err
}

// CallMemoryList asks for one page of an agent's facts.
func CallMemoryList(ctx context.Context, socket, tcp, agent, cursor string, limit int) (MemoryListResponse, bool, error) {
	params := url.Values{}
	setIf(params, "cursor", cursor)
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	target := memoryTarget(agent, "")
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var out MemoryListResponse
	online, err := callControl(ctx, socket, tcp, http.MethodGet, target, nil, &out)
	return out, online, err
}

// CallMemoryGet reads one fact with its content and version.
func CallMemoryGet(ctx context.Context, socket, tcp, agent, name string) (memory.Fact, bool, error) {
	var out memory.Fact
	online, err := callControl(ctx, socket, tcp, http.MethodGet, memoryTarget(agent, name), nil, &out)
	return out, online, err
}

// CallMemoryPut creates or updates one fact. A refused expected_version
// comes back as an error carrying the daemon's 409 body, current version
// included; the call never replays.
func CallMemoryPut(ctx context.Context, socket, tcp, agent, name string, q MemoryPutRequest) (memory.Fact, bool, error) {
	body, err := json.Marshal(q)
	if err != nil {
		return memory.Fact{}, false, err
	}
	var out memory.Fact
	online, err := callControl(ctx, socket, tcp, http.MethodPut, memoryTarget(agent, name), body, &out)
	return out, online, err
}

// CallMemoryDelete removes one fact; confirm must already be true, the
// CLI having asked for --confirm.
func CallMemoryDelete(ctx context.Context, socket, tcp, agent, name string, confirm bool) (MemoryDeleteResponse, bool, error) {
	body, err := json.Marshal(MemoryDeleteRequest{Confirm: confirm})
	if err != nil {
		return MemoryDeleteResponse{}, false, err
	}
	var out MemoryDeleteResponse
	online, err := callControl(ctx, socket, tcp, http.MethodDelete, memoryTarget(agent, name), body, &out)
	return out, online, err
}

// CallMemorySearch runs one memory search through the running daemon.
func CallMemorySearch(ctx context.Context, socket, tcp string, opt memory.SearchOptions) (memory.SearchResult, bool, error) {
	params := url.Values{}
	params.Set("q", opt.Query)
	setIf(params, "agent", opt.Agent)
	setIf(params, "mode", opt.Mode)
	if !opt.IncludeShared {
		params.Set("include_shared", "0")
	}
	if opt.TopK > 0 {
		params.Set("top_k", strconv.Itoa(opt.TopK))
	}
	var out memory.SearchResult
	online, err := callControl(ctx, socket, tcp, http.MethodGet, "/memory/search?"+params.Encode(), nil, &out)
	return out, online, err
}

// CallMemoryMigrate moves the memory tree to layout v2 under owner
// through the running daemon.
func CallMemoryMigrate(ctx context.Context, socket, tcp, owner string, confirm bool) (MemoryMigrateResponse, bool, error) {
	body, err := json.Marshal(MemoryMigrateRequest{Owner: owner, Confirm: confirm})
	if err != nil {
		return MemoryMigrateResponse{}, false, err
	}
	var out MemoryMigrateResponse
	online, err := callControl(ctx, socket, tcp, http.MethodPost, "/memory/migrate", body, &out)
	return out, online, err
}
