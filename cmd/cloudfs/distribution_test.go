package main

import "testing"

func TestMCPHTTPFlagAcceptsOptionalAddress(t *testing.T) {
	withAddress := parseFlags([]string{"--http", "0.0.0.0:8765", "--read-only"}, "stdio", "read-only")
	if got := withAddress.str("http", ""); got != "0.0.0.0:8765" {
		t.Fatalf("--http address = %q", got)
	}
	if !withAddress.bool("read-only") || len(withAddress.args) != 0 {
		t.Fatalf("flags parsed incorrectly: %#v", withAddress)
	}

	withoutAddress := parseFlags([]string{"--http"}, "stdio", "read-only")
	if !withoutAddress.bool("http") || withoutAddress.str("http", "configured") != "configured" {
		t.Fatalf("value-less --http parsed incorrectly: %#v", withoutAddress)
	}
}

func TestMCPHTTPTokenComesFromEnvironment(t *testing.T) {
	t.Setenv("CLOUDFS_MCP_TOKEN", "container-secret")
	if got := mcpHTTPToken(); got != "container-secret" {
		t.Fatalf("token = %q", got)
	}
}
