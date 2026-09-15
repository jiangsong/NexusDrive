package daemon

import (
	"context"
	"testing"
	"time"
)

// TestCollectorReportsTheMCPListenerAsItChanges: the collector is built
// before the HTTP transport comes up, so the view has to read the listener
// state on every call; the owner flag is the journal's, and the snippets
// only appear once cmd/cloudfs injects a renderer.
func TestCollectorReportsTheMCPListenerAsItChanges(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	col := d.Collector()
	if col.MCP == nil {
		t.Fatal("collector lacks the MCP view")
	}
	c := col.MCP.Connect(ctx)
	if c.HTTPListening || c.URL != "" || !c.Owner || c.AuthRequired || len(c.Snippets) != 0 {
		t.Fatalf("before the listener: %+v", c)
	}
	d.SetMCPHTTP("127.0.0.1:8765", true)
	c = col.MCP.Connect(ctx)
	if !c.HTTPListening || c.HTTPAddr != "127.0.0.1:8765" || c.URL != "http://127.0.0.1:8765/" || !c.AuthRequired || len(c.Snippets) != 0 {
		t.Fatalf("after the listener: %+v", c)
	}
	d.SetMCPSnippets(func(url string) (map[string]string, map[string]string) {
		return map[string]string{"claude": url + " <token>"}, map[string]string{"claude": "add " + url}
	})
	c = col.MCP.Connect(ctx)
	if c.Snippets["claude"] != "http://127.0.0.1:8765/ <token>" || c.AddCommands["claude"] != "add http://127.0.0.1:8765/" {
		t.Fatalf("snippets: %+v", c)
	}
	d.SetMCPHTTP("", false)
	if c = col.MCP.Connect(ctx); c.HTTPListening || len(c.Snippets) != 0 {
		t.Fatalf("after the listener went away: %+v", c)
	}

	other, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if c := other.Collector().MCP.Connect(ctx); c.Owner {
		t.Fatalf("the non-owner must say so: %+v", c)
	}
}
